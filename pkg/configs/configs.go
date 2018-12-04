package configs

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/rulefmt"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"

	legacy_promql "github.com/cortexproject/cortex/pkg/configs/legacy_promql"
	"github.com/cortexproject/cortex/pkg/util"
)

// An ID is the ID of a single users's Cortex configuration. When a
// configuration changes, it gets a new ID.
type ID int

// RuleFormatVersion indicates which Prometheus rule format (v1 vs. v2) to use in parsing.
type RuleFormatVersion int

const (
	// RuleFormatV1 is the Prometheus 1.x rule format.
	RuleFormatV1 RuleFormatVersion = iota
	// RuleFormatV2 is the Prometheus 2.x rule format.
	RuleFormatV2 RuleFormatVersion = iota
)

// IsValid returns whether the rules format version is a valid (known) version.
func (v RuleFormatVersion) IsValid() bool {
	switch v {
	case RuleFormatV1, RuleFormatV2:
		return true
	default:
		return false
	}
}

// MarshalJSON implements json.Marshaler.
func (v RuleFormatVersion) MarshalJSON() ([]byte, error) {
	switch v {
	case RuleFormatV1:
		return json.Marshal("1")
	case RuleFormatV2:
		return json.Marshal("2")
	default:
		return nil, fmt.Errorf("unknown rule format version %d", v)
	}
}

// UnmarshalJSON implements json.Unmarshaler.
func (v *RuleFormatVersion) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "1":
		*v = RuleFormatV1
	case "2":
		*v = RuleFormatV2
	default:
		return fmt.Errorf("unknown rule format version %q", string(data))
	}
	return nil
}

// A Config is a Cortex configuration for a single user.
type Config struct {
	// RulesFiles maps from a rules filename to file contents.
	RulesConfig        RulesConfig
	AlertmanagerConfig string
}

// configCompat is a compatibility struct to support old JSON config blobs
// saved in the config DB that didn't have a rule format version yet and
// just had a top-level field for the rule files.
type configCompat struct {
	RulesFiles         map[string]string `json:"rules_files"`
	RuleFormatVersion  RuleFormatVersion `json:"rule_format_version"`
	AlertmanagerConfig string            `json:"alertmanager_config"`
}

// MarshalJSON implements json.Marshaler.
func (c Config) MarshalJSON() ([]byte, error) {
	compat := &configCompat{
		RulesFiles:         c.RulesConfig.Files,
		RuleFormatVersion:  c.RulesConfig.FormatVersion,
		AlertmanagerConfig: c.AlertmanagerConfig,
	}

	return json.Marshal(compat)
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *Config) UnmarshalJSON(data []byte) error {
	compat := configCompat{}
	if err := json.Unmarshal(data, &compat); err != nil {
		return err
	}
	*c = Config{
		RulesConfig: RulesConfig{
			Files:         compat.RulesFiles,
			FormatVersion: compat.RuleFormatVersion,
		},
		AlertmanagerConfig: compat.AlertmanagerConfig,
	}
	return nil
}

// View is what's returned from the Weave Cloud configs service
// when we ask for all Cortex configurations.
//
// The configs service is essentially a JSON blob store that gives each
// _version_ of a configuration a unique ID and guarantees that later versions
// have greater IDs.
type View struct {
	ID        ID        `json:"id"`
	Config    Config    `json:"config"`
	DeletedAt time.Time `json:"deleted_at"`
}

// GetVersionedRulesConfig specializes the view to just the rules config.
func (v View) GetVersionedRulesConfig() *VersionedRulesConfig {
	if v.Config.RulesConfig.Files == nil {
		return nil
	}
	return &VersionedRulesConfig{
		ID:        v.ID,
		Config:    v.Config.RulesConfig,
		DeletedAt: v.DeletedAt,
	}
}

// RulesConfig is the rules configuration for a particular organization.
type RulesConfig struct {
	FormatVersion RuleFormatVersion `json:"format_version"`
	Files         map[string]string `json:"files"`
}

// Equal compares two RulesConfigs for equality.
//
// instance Eq RulesConfig
func (c RulesConfig) Equal(o RulesConfig) bool {
	if c.FormatVersion != o.FormatVersion {
		return false
	}
	if len(o.Files) != len(c.Files) {
		return false
	}
	for k, v1 := range c.Files {
		v2, ok := o.Files[k]
		if !ok || v1 != v2 {
			return false
		}
	}
	return true
}

// Parse parses and validates the content of the rule files in a RulesConfig
// according to the passed rule format version.
func (c RulesConfig) Parse() (map[string][]rules.Rule, error) {
	switch c.FormatVersion {
	case RuleFormatV1:
		return c.parseV1()
	case RuleFormatV2:
		return c.parseV2()
	default:
		return nil, fmt.Errorf("unknown rule format version %v", c.FormatVersion)
	}
}

// parseV2 parses and validates the content of the rule files in a RulesConfig
// according to the Prometheus 2.x rule format.
//
// NOTE: On one hand, we cannot return fully-fledged lists of rules.Group
// here yet, as creating a rules.Group requires already
// passing in rules.ManagerOptions options (which in turn require a
// notifier, appender, etc.), which we do not want to create simply
// for parsing. On the other hand, we should not return barebones
// rulefmt.RuleGroup sets here either, as only a fully-converted rules.Rule
// is able to track alert states over multiple rule evaluations. The caller
// would otherwise have to ensure to convert the rulefmt.RuleGroup only exactly
// once, not for every evaluation (or risk losing alert pending states). So
// it's probably better to just return a set of rules.Rule here.
func (c RulesConfig) parseV2() (map[string][]rules.Rule, error) {
	groups := map[string][]rules.Rule{}

	for fn, content := range c.Files {
		rgs, errs := rulefmt.Parse([]byte(content))
		if len(errs) > 0 {
			return nil, fmt.Errorf("error parsing %s: %v", fn, errs[0])
		}

		for _, rg := range rgs.Groups {
			rls := make([]rules.Rule, 0, len(rg.Rules))
			for _, rl := range rg.Rules {
				expr, err := promql.ParseExpr(rl.Expr)
				if err != nil {
					return nil, err
				}

				if rl.Alert != "" {
					rls = append(rls, rules.NewAlertingRule(
						rl.Alert,
						expr,
						time.Duration(rl.For),
						labels.FromMap(rl.Labels),
						labels.FromMap(rl.Annotations),
						true,
						log.With(util.Logger, "alert", rl.Alert),
					))
					continue
				}
				rls = append(rls, rules.NewRecordingRule(
					rl.Record,
					expr,
					labels.FromMap(rl.Labels),
				))
			}

			// Group names have to be unique in Prometheus, but only within one rules file.
			groups[rg.Name+";"+fn] = rls
		}
	}

	return groups, nil
}

// parseV1 parses and validates the content of the rule files in a RulesConfig
// according to the Prometheus 1.x rule format.
//
// The same comment about rule groups as on ParseV2() applies here.
func (c RulesConfig) parseV1() (map[string][]rules.Rule, error) {
	result := map[string][]rules.Rule{}
	for fn, content := range c.Files {
		stmts, err := legacy_promql.ParseStmts(content)
		if err != nil {
			return nil, fmt.Errorf("error parsing %s: %s", fn, err)
		}
		ra := []rules.Rule{}
		for _, stmt := range stmts {
			var rule rules.Rule

			switch r := stmt.(type) {
			case *legacy_promql.AlertStmt:
				// Re-parse the expression to get it into the right types.
				expr, err := promql.ParseExpr(r.Expr.String())
				if err != nil {
					return nil, err
				}

				rule = rules.NewAlertingRule(r.Name, expr, r.Duration, r.Labels, r.Annotations, true, util.Logger)

			case *legacy_promql.RecordStmt:
				// Re-parse the expression to get it into the right types.
				expr, err := promql.ParseExpr(r.Expr.String())
				if err != nil {
					return nil, err
				}

				rule = rules.NewRecordingRule(r.Name, expr, r.Labels)

			default:
				return nil, fmt.Errorf("ruler.GetRules: unknown statement type")
			}
			ra = append(ra, rule)
		}
		result[fn] = ra
	}
	return result, nil
}

// VersionedRulesConfig is a RulesConfig together with a version.
// `data Versioned a = Versioned { id :: ID , config :: a }`
type VersionedRulesConfig struct {
	ID        ID          `json:"id"`
	Config    RulesConfig `json:"config"`
	DeletedAt time.Time   `json:"deleted_at"`
}

// IsDeleted tells you if the config is deleted.
func (vr VersionedRulesConfig) IsDeleted() bool {
	return !vr.DeletedAt.IsZero()
}
