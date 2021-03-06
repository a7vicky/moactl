/*
Copyright (c) 2020 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	"github.com/spf13/cobra"

	"github.com/openshift/moactl/pkg/aws"
	c "github.com/openshift/moactl/pkg/cluster"
	"github.com/openshift/moactl/pkg/interactive"
	"github.com/openshift/moactl/pkg/logging"
	"github.com/openshift/moactl/pkg/ocm"
	"github.com/openshift/moactl/pkg/ocm/upgrades"
	"github.com/openshift/moactl/pkg/ocm/versions"
	rprtr "github.com/openshift/moactl/pkg/reporter"
)

var args struct {
	clusterKey           string
	version              string
	scheduleDate         string
	scheduleTime         string
	nodeDrainGracePeriod string
}

var Cmd = &cobra.Command{
	Use:   "cluster",
	Short: "Upgrade cluster",
	Long:  "Upgrade cluster to a new available version",
	Example: `  # Interactively schedule an upgrade on the cluster named "mycluster"
  rosa upgrade cluster --cluster=mycluster --interactive

  # Schedule a cluster upgrade within the hour
  rosa upgade cluster -c mycluster --version 4.5.20`,
	Run: run,
}

func init() {
	flags := Cmd.Flags()
	flags.SortFlags = false

	flags.StringVarP(
		&args.clusterKey,
		"cluster",
		"c",
		"",
		"Name or ID of the cluster to schedule the upgrade for (required)",
	)
	Cmd.MarkFlagRequired("cluster")

	flags.StringVar(
		&args.version,
		"version",
		"",
		"Version of OpenShift that the cluster will be upgraded to",
	)

	flags.StringVar(
		&args.scheduleDate,
		"schedule-date",
		"",
		"Next date the upgrade should run at the specified time. Format should be 'yyyy-mm-dd'",
	)

	flags.StringVar(
		&args.scheduleTime,
		"schedule-time",
		"",
		"Next time the upgrade should run on the specified date. Format should be 'HH:mm'",
	)

	flags.StringVar(
		&args.nodeDrainGracePeriod,
		"node-drain-grace-period",
		"1 hour",
		"You may set a grace period for how long Pod Disruption Budget-protected workloads will be "+
			"respected during upgrades.\nAfter this grace period, any workloads protected by Pod Disruption "+
			"Budgets that have not been successfully drained from a node will be forcibly evicted",
	)
}

func run(cmd *cobra.Command, _ []string) {
	reporter := rprtr.CreateReporterOrExit()
	logger := logging.CreateLoggerOrExit(reporter)

	// Check that the cluster key (name, identifier or external identifier) given by the user
	// is reasonably safe so that there is no risk of SQL injection:
	clusterKey := args.clusterKey
	if !c.IsValidClusterKey(clusterKey) {
		reporter.Errorf(
			"Cluster name, identifier or external identifier '%s' isn't valid: it "+
				"must contain only letters, digits, dashes and underscores",
			clusterKey,
		)
		os.Exit(1)
	}

	// Create the AWS client:
	var err error
	awsClient, err := aws.NewClient().
		Logger(logger).
		Build()
	if err != nil {
		reporter.Errorf("Failed to create AWS client: %v", err)
		os.Exit(1)
	}

	awsCreator, err := awsClient.GetCreator()
	if err != nil {
		reporter.Errorf("Failed to get AWS creator: %v", err)
		os.Exit(1)
	}

	// Create the client for the OCM API:
	ocmConnection, err := ocm.NewConnection().
		Logger(logger).
		Build()
	if err != nil {
		reporter.Errorf("Failed to create OCM connection: %v", err)
		os.Exit(1)
	}
	defer func() {
		err = ocmConnection.Close()
		if err != nil {
			reporter.Errorf("Failed to close OCM connection: %v", err)
		}
	}()

	// Get the client for the OCM collection of clusters:
	ocmClient := ocmConnection.ClustersMgmt().V1()

	// Try to find the cluster:
	reporter.Debugf("Loading cluster '%s'", clusterKey)
	cluster, err := ocm.GetCluster(ocmClient.Clusters(), clusterKey, awsCreator.ARN)
	if err != nil {
		reporter.Errorf("Failed to get cluster '%s': %v", clusterKey, err)
		os.Exit(1)
	}

	if cluster.State() != cmv1.ClusterStateReady {
		reporter.Errorf("Cluster '%s' is not yet ready", clusterKey)
		os.Exit(1)
	}

	scheduledUpgrade, err := upgrades.GetScheduledUpgrade(ocmClient, cluster.ID())
	if err != nil {
		reporter.Errorf("Failed to get scheduled upgrades for cluster '%s': %v", clusterKey, err)
		os.Exit(1)
	}
	if scheduledUpgrade != nil {
		reporter.Warnf("There is already a scheduled upgrade to version %s on %s",
			scheduledUpgrade.Version(),
			scheduledUpgrade.NextRun().Format("2006-01-02 15:04 MST"),
		)
		os.Exit(0)
	}

	version := args.version
	scheduleDate := args.scheduleDate
	scheduleTime := args.scheduleTime

	availableUpgrades, err := versions.GetAvailableUpgrades(ocmClient, versions.GetVersionID(cluster))
	if err != nil {
		reporter.Errorf("Failed to find available upgrades: %v", err)
		os.Exit(1)
	}
	if len(availableUpgrades) == 0 {
		reporter.Warnf("There are no available upgrades")
		os.Exit(0)
	}

	if version == "" || interactive.Enabled() {
		if version == "" {
			version = availableUpgrades[0]
		}
		version, err = interactive.GetOption(interactive.Input{
			Question: "Version",
			Help:     cmd.Flags().Lookup("version").Usage,
			Options:  availableUpgrades,
			Default:  version,
			Required: true,
		})
		if err != nil {
			reporter.Errorf("Expected a valid version to upgrade to: %s", err)
			os.Exit(1)
		}
	}

	// Check that the version is valid
	validVersion := false
	for _, v := range availableUpgrades {
		if v == version {
			validVersion = true
			break
		}
	}
	if !validVersion {
		reporter.Errorf("Expected a valid version to upgrade to")
		os.Exit(1)
	}

	// Set the default next run within the next 10 minutes
	now := time.Now().UTC().Add(time.Minute * 10)
	if scheduleDate == "" {
		scheduleDate = now.Format("2006-01-02")
	}
	if scheduleTime == "" {
		scheduleTime = now.Format("15:04")
	}

	if interactive.Enabled() {
		// If datetimes are set, use them in the interactive form, otherwise fallback to 'now'
		scheduleParsed, err := time.Parse("2006-01-02 15:04", fmt.Sprintf("%s %s", scheduleDate, scheduleTime))
		if err != nil {
			scheduleParsed = now
		}
		scheduleDate = scheduleParsed.Format("2006-01-02")
		scheduleTime = scheduleParsed.Format("15:04")

		scheduleDate, err = interactive.GetString(interactive.Input{
			Question: "Please input desired date in format yyyy-mm-dd",
			Default:  scheduleDate,
			Required: true,
		})
		if err != nil {
			reporter.Errorf("Expected a valid date: %s", err)
			os.Exit(1)
		}
		_, err = time.Parse("2006-01-02", scheduleDate)
		if err != nil {
			reporter.Errorf("Date format '%s' invalid", scheduleDate)
			os.Exit(1)
		}

		scheduleTime, err = interactive.GetString(interactive.Input{
			Question: "Please input desired UTC time in format HH:mm",
			Default:  scheduleTime,
			Required: true,
		})
		if err != nil {
			reporter.Errorf("Expected a valid time: %s", err)
			os.Exit(1)
		}
		_, err = time.Parse("15:04", scheduleTime)
		if err != nil {
			reporter.Errorf("Time format '%s' invalid", scheduleTime)
			os.Exit(1)
		}
	}

	// Parse next run to time.Time
	nextRun, err := time.Parse("2006-01-02 15:04", fmt.Sprintf("%s %s", scheduleDate, scheduleTime))
	if err != nil {
		reporter.Errorf("Time format invalid: %s", err)
		os.Exit(1)
	}

	upgradePolicyBuilder := cmv1.NewUpgradePolicy().
		ScheduleType("manual").
		Version(version).
		NextRun(nextRun)

	nodeDrainGracePeriod := ""
	// Determine if the cluster already has a node drain grace period set and use that as the default
	nd := cluster.NodeDrainGracePeriod()
	if _, ok := nd.GetValue(); ok {
		// Convert larger times to hours, since the API only stores minutes
		val := int(nd.Value())
		unit := nd.Unit()
		if val >= 60 {
			val = val / 60
			if val == 1 {
				unit = "hour"
			} else {
				unit = "hours"
			}
		}
		nodeDrainGracePeriod = fmt.Sprintf("%d %s", val, unit)
	}
	// If node drain grace period is not set, or the user sent it as a CLI argument, use that instead
	if nodeDrainGracePeriod == "" || cmd.Flags().Changed("node-drain-grace-period") {
		nodeDrainGracePeriod = args.nodeDrainGracePeriod
	}
	nodeDrainOptions := []string{
		"15 minutes",
		"30 minutes",
		"45 minutes",
		"1 hour",
		"2 hours",
		"4 hours",
		"8 hours",
	}
	if interactive.Enabled() {
		nodeDrainGracePeriod, err = interactive.GetOption(interactive.Input{
			Question: "Node draining",
			Help:     cmd.Flags().Lookup("node-drain-grace-period").Usage,
			Options:  nodeDrainOptions,
			Default:  nodeDrainGracePeriod,
			Required: true,
		})
		if err != nil {
			reporter.Errorf("Expected a valid node drain grace period: %s", err)
			os.Exit(1)
		}
	}
	nodeDrainParsed := strings.Split(nodeDrainGracePeriod, " ")
	nodeDrainValue, err := strconv.ParseFloat(nodeDrainParsed[0], 64)
	if err != nil {
		reporter.Errorf("Expected a valid node drain grace period: %s", err)
		os.Exit(1)
	}
	if nodeDrainParsed[1] == "hours" || nodeDrainParsed[1] == "hour" {
		nodeDrainValue = nodeDrainValue * 60
	}

	clusterSpec, err := cmv1.NewCluster().
		NodeDrainGracePeriod(cmv1.NewValue().
			Value(nodeDrainValue).
			Unit("minutes")).
		Build()
	if err != nil {
		reporter.Errorf("Failed to update cluster '%s': %v", clusterKey, err)
		os.Exit(1)
	}

	upgradePolicy, err := upgradePolicyBuilder.Build()
	if err != nil {
		reporter.Errorf("Failed to schedule upgrade for cluster '%s': %v", clusterKey, err)
		os.Exit(1)
	}

	_, err = ocmClient.Clusters().
		Cluster(cluster.ID()).
		UpgradePolicies().
		Add().
		Body(upgradePolicy).
		Send()
	if err != nil {
		reporter.Errorf("Failed to schedule upgrade for cluster '%s': %v", clusterKey, err)
		os.Exit(1)
	}

	_, err = ocmClient.Clusters().
		Cluster(cluster.ID()).
		Update().
		Body(clusterSpec).
		Send()
	if err != nil {
		reporter.Errorf("Failed to update cluster '%s': %v", clusterKey, err)
		os.Exit(1)
	}

	reporter.Infof("Upgrade successfully scheduled for cluster '%s'", clusterKey)
}
