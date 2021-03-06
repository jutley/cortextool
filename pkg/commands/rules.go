package commands

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/alecthomas/chroma/quick"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/grafana/cortextool/pkg/client"
	"github.com/grafana/cortextool/pkg/rules"
	log "github.com/sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"
)

var (
	ruleLoadTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "cortex",
		Name:      "last_rule_load_timestamp_seconds",
		Help:      "The timestamp of the last rule load.",
	})
	ruleLoadSuccessTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "cortex",
		Name:      "last_rule_load_success_timestamp_seconds",
		Help:      "The timestamp of the last successful rule load.",
	})
)

// RuleCommand configures and executes rule related cortex api operations
type RuleCommand struct {
	ClientConfig client.Config

	cli *client.CortexClient

	// Get Rule Groups Configs
	Namespace string
	RuleGroup string

	// Load Rules Configs
	RuleFiles []string
}

// Register rule related commands and flags with the kingpin application
func (r *RuleCommand) Register(app *kingpin.Application) {
	rulesCmd := app.Command("rules", "View & edit rules stored in cortex.").PreAction(r.setup)
	rulesCmd.Flag("address", "Address of the cortex cluster, alternatively set CORTEX_ADDRESS.").Envar("CORTEX_ADDRESS").Required().StringVar(&r.ClientConfig.Address)
	rulesCmd.Flag("id", "Cortex tenant id, alternatively set CORTEX_TENTANT_ID.").Envar("CORTEX_TENTANT_ID").Required().StringVar(&r.ClientConfig.ID)
	rulesCmd.Flag("key", "Api key to use when contacting cortex, alternatively set $CORTEX_API_KEY.").Default("").Envar("CORTEX_API_KEY").StringVar(&r.ClientConfig.Key)

	// List Rules Command
	rulesCmd.Command("list", "List the rules currently in the cortex ruler.").Action(r.listRules)

	// Print Rules Command
	rulesCmd.Command("print", "Print the rules currently in the cortex ruler.").Action(r.printRules)

	// Get RuleGroup Command
	getRuleGroupCmd := rulesCmd.Command("get", "Retreive a rulegroup from the ruler.").Action(r.getRuleGroup)
	getRuleGroupCmd.Arg("namespace", "Namespace of the rulegroup to retrieve.").Required().StringVar(&r.Namespace)
	getRuleGroupCmd.Arg("group", "Name of the rulegroup ot retrieve.").Required().StringVar(&r.RuleGroup)

	// Delete RuleGroup Command
	deleteRuleGroupCmd := rulesCmd.Command("delete", "Delete a rulegroup from the ruler.").Action(r.deleteRuleGroup)
	deleteRuleGroupCmd.Arg("namespace", "Namespace of the rulegroup to delete.").Required().StringVar(&r.Namespace)
	deleteRuleGroupCmd.Arg("group", "Name of the rulegroup ot delete.").Required().StringVar(&r.RuleGroup)

	loadRulesCmd := rulesCmd.Command("load", "load a set of rules to a designated cortex endpoint").Action(r.loadRules)
	loadRulesCmd.Arg("rule-files", "The rule files to check.").Required().ExistingFilesVar(&r.RuleFiles)
}

func (r *RuleCommand) setup(k *kingpin.ParseContext) error {
	prometheus.MustRegister(
		ruleLoadTimestamp,
		ruleLoadSuccessTimestamp,
	)

	cli, err := client.New(r.ClientConfig)
	if err != nil {
		return err
	}
	r.cli = cli

	return nil
}

func (r *RuleCommand) listRules(k *kingpin.ParseContext) error {
	rules, err := r.cli.ListRules(context.Background(), "")
	if err != nil {
		log.Fatalf("unable to read rules from cortex, %v", err)

	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', tabwriter.Debug)

	fmt.Fprintln(w, "Namespace\t Rule Group")
	for ns, rulegroups := range rules {
		for _, rg := range rulegroups {
			fmt.Fprintf(w, "%s\t %s\n", ns, rg.Name)
		}
	}

	w.Flush()

	return nil
}

func (r *RuleCommand) printRules(k *kingpin.ParseContext) error {
	rules, err := r.cli.ListRules(context.Background(), "")
	if err != nil {
		if err == client.ErrResourceNotFound {
			log.Infof("no rule groups currently exist for this user")
			return nil
		}
		log.Fatalf("unable to read rules from cortex, %v", err)
	}
	d, err := yaml.Marshal(&rules)
	if err != nil {
		return err
	}

	err = quick.Highlight(os.Stdout, string(d), "yaml", "terminal", "swapoff")
	if err != nil {
		return err
	}

	return nil
}

func (r *RuleCommand) getRuleGroup(k *kingpin.ParseContext) error {
	group, err := r.cli.GetRuleGroup(context.Background(), r.Namespace, r.RuleGroup)
	if err != nil {
		if err == client.ErrResourceNotFound {
			log.Infof("this rule group does not currently exist")
			return nil
		}
		log.Fatalf("unable to read rules from cortex, %v", err)
	}
	d, err := yaml.Marshal(&group)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	err = quick.Highlight(os.Stdout, string(d), "yaml", "terminal", "swapoff")
	if err != nil {
		return err
	}

	return nil
}

func (r *RuleCommand) deleteRuleGroup(k *kingpin.ParseContext) error {
	err := r.cli.DeleteRuleGroup(context.Background(), r.Namespace, r.RuleGroup)
	if err != nil {
		log.Fatalf("unable to delete rule group from cortex, %v", err)
	}
	return nil
}

func (r *RuleCommand) loadRules(k *kingpin.ParseContext) error {
	nss, err := rules.ParseFiles(r.RuleFiles)
	if err != nil {
		return errors.Wrap(err, "load operation unsuccessful, unable to parse rules files")
	}
	ruleLoadTimestamp.SetToCurrentTime()

	for _, ns := range nss {
		for _, group := range ns.Groups {
			curGroup, err := r.cli.GetRuleGroup(context.Background(), ns.Namespace, group.Name)
			if err != nil && err != client.ErrResourceNotFound {
				return errors.Wrap(err, "load operation unsuccessful, unable to contact cortex api")
			}
			if curGroup != nil {
				err = rules.CompareGroups(*curGroup, group)
				if err == nil {
					log.WithFields(log.Fields{
						"group":     group.Name,
						"namespace": ns.Namespace,
					}).Infof("group already exists")
					continue
				}
				log.WithFields(log.Fields{
					"group":      group.Name,
					"namespace":  ns.Namespace,
					"difference": err,
				}).Infof("updating group")
			}

			err = r.cli.CreateRuleGroup(context.Background(), ns.Namespace, group)
			if err != nil {
				log.WithError(err).WithFields(log.Fields{
					"group":     group.Name,
					"namespace": ns.Namespace,
				}).Errorf("unable to load rule group")
				return fmt.Errorf("load operation unsuccessful")
			}
		}
	}

	ruleLoadSuccessTimestamp.SetToCurrentTime()
	return nil
}
