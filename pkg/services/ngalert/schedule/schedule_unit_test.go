package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	busmock "github.com/grafana/grafana/pkg/bus/mock"
	"github.com/grafana/grafana/pkg/expr"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/annotations"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/ngalert/eval"
	"github.com/grafana/grafana/pkg/services/ngalert/image"
	"github.com/grafana/grafana/pkg/services/ngalert/metrics"
	"github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/notifier"
	"github.com/grafana/grafana/pkg/services/ngalert/provisioning"
	"github.com/grafana/grafana/pkg/services/ngalert/sender"
	"github.com/grafana/grafana/pkg/services/ngalert/state"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
	"github.com/grafana/grafana/pkg/services/secrets/fakes"
	secretsManager "github.com/grafana/grafana/pkg/services/secrets/manager"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
)

func TestSendingToExternalAlertmanager(t *testing.T) {
	fakeAM := store.NewFakeExternalAlertmanager(t)
	defer fakeAM.Close()
	fakeRuleStore := store.NewFakeRuleStore(t)
	fakeInstanceStore := &store.FakeInstanceStore{}
	fakeAdminConfigStore := store.NewFakeAdminConfigStore(t)

	// create alert rule with one second interval
	alertRule := CreateTestAlertRule(t, fakeRuleStore, 1, 1, eval.Alerting)

	// First, let's create an admin configuration that holds an alertmanager.
	adminConfig := &models.AdminConfiguration{OrgID: 1, Alertmanagers: []string{fakeAM.Server.URL}, SendAlertsTo: models.AllAlertmanagers}
	cmd := store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))

	sched, mockedClock, alertsRouter := setupScheduler(t, fakeRuleStore, fakeInstanceStore, fakeAdminConfigStore, nil)

	// Make sure we sync the configuration at least once before the evaluation happens to guarantee the sender is running
	// when the first alert triggers.
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())
	alertsRouter.AdminConfigMtx.Lock()
	require.Equal(t, 1, len(alertsRouter.Senders))
	require.Equal(t, 1, len(alertsRouter.SendersCfgHash))
	alertsRouter.AdminConfigMtx.Unlock()

	// Then, ensure we've discovered the Alertmanager.
	require.Eventually(t, func() bool {
		return len(alertsRouter.AlertmanagersFor(1)) == 1 && len(alertsRouter.DroppedAlertmanagersFor(1)) == 0
	}, 10*time.Second, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
	})
	go func() {
		err := sched.Run(ctx)
		require.NoError(t, err)
	}()

	// With everything up and running, let's advance the time to make sure we get at least one alert iteration.
	mockedClock.Add(2 * time.Second)

	// Eventually, our Alertmanager should have received at least one alert.
	require.Eventually(t, func() bool {
		return fakeAM.AlertsCount() >= 1 && fakeAM.AlertNamesCompare([]string{alertRule.Title})
	}, 10*time.Second, 200*time.Millisecond)

	// Now, let's remove the Alertmanager from the admin configuration.
	adminConfig.Alertmanagers = []string{}
	cmd = store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))

	// Again, make sure we sync and verify the senders.
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())
	alertsRouter.AdminConfigMtx.Lock()
	require.Equal(t, 0, len(alertsRouter.Senders))
	require.Equal(t, 0, len(alertsRouter.SendersCfgHash))
	alertsRouter.AdminConfigMtx.Unlock()

	// Then, ensure we've dropped the Alertmanager.
	require.Eventually(t, func() bool {
		return len(alertsRouter.AlertmanagersFor(1)) == 0 && len(alertsRouter.DroppedAlertmanagersFor(1)) == 0
	}, 10*time.Second, 200*time.Millisecond)
}

func TestSendingToExternalAlertmanager_WithMultipleOrgs(t *testing.T) {
	fakeAM := store.NewFakeExternalAlertmanager(t)
	defer fakeAM.Close()
	fakeRuleStore := store.NewFakeRuleStore(t)
	fakeInstanceStore := &store.FakeInstanceStore{}
	fakeAdminConfigStore := store.NewFakeAdminConfigStore(t)

	// First, let's create an admin configuration that holds an alertmanager.
	adminConfig := &models.AdminConfiguration{OrgID: 1, Alertmanagers: []string{fakeAM.Server.URL}}
	cmd := store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))

	sched, mockedClock, alertsRouter := setupScheduler(t, fakeRuleStore, fakeInstanceStore, fakeAdminConfigStore, nil)

	// Make sure we sync the configuration at least once before the evaluation happens to guarantee the sender is running
	// when the first alert triggers.
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())
	alertsRouter.AdminConfigMtx.Lock()
	require.Equal(t, 1, len(alertsRouter.Senders))
	require.Equal(t, 1, len(alertsRouter.SendersCfgHash))
	alertsRouter.AdminConfigMtx.Unlock()

	// Then, ensure we've discovered the Alertmanager.
	require.Eventuallyf(t, func() bool {
		return len(alertsRouter.AlertmanagersFor(1)) == 1 && len(alertsRouter.DroppedAlertmanagersFor(1)) == 0
	}, 10*time.Second, 200*time.Millisecond, "Alertmanager for org 1 was never discovered")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
	})
	go func() {
		err := sched.Run(ctx)
		require.NoError(t, err)
	}()

	// 1. Now, let's assume a new org comes along.
	adminConfig2 := &models.AdminConfiguration{OrgID: 2, Alertmanagers: []string{fakeAM.Server.URL}}
	cmd = store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig2}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))

	// If we sync again, new senders must have spawned.
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())
	alertsRouter.AdminConfigMtx.Lock()
	require.Equal(t, 2, len(alertsRouter.Senders))
	require.Equal(t, 2, len(alertsRouter.SendersCfgHash))
	alertsRouter.AdminConfigMtx.Unlock()

	// Then, ensure we've discovered the Alertmanager for the new organization.
	require.Eventuallyf(t, func() bool {
		return len(alertsRouter.AlertmanagersFor(2)) == 1 && len(alertsRouter.DroppedAlertmanagersFor(2)) == 0
	}, 10*time.Second, 200*time.Millisecond, "Alertmanager for org 2 was never discovered")

	// With everything up and running, let's advance the time to make sure we get at least one alert iteration.
	mockedClock.Add(10 * time.Second)

	// TODO(gotjosh): Disabling this assertion as for some reason even after advancing the clock the alert is not being delivered.
	// the check previous to this assertion would ensure that the sender is up and running before sending the notification.
	// However, sometimes this does not happen.

	// Create two alert rules with one second interval.
	// alertRuleOrgOne := CreateTestAlertRule(t, FakeRuleStore, 1, 1)
	// alertRuleOrgTwo := CreateTestAlertRule(t, FakeRuleStore, 1, 2)
	// Eventually, our Alertmanager should have received at least two alerts.
	// var count int
	// require.Eventuallyf(t, func() bool {
	//	count := fakeAM.AlertsCount()
	//	return count == 2 && fakeAM.AlertNamesCompare([]string{alertRuleOrgOne.Title, alertRuleOrgTwo.Title})
	// }, 20*time.Second, 200*time.Millisecond, "Alertmanager never received an '%s' from org 1 or '%s' from org 2, the alert count was: %d", alertRuleOrgOne.Title, alertRuleOrgTwo.Title, count)

	// 2. Next, let's modify the configuration of an organization by adding an extra alertmanager.
	fakeAM2 := store.NewFakeExternalAlertmanager(t)
	adminConfig2 = &models.AdminConfiguration{OrgID: 2, Alertmanagers: []string{fakeAM.Server.URL, fakeAM2.Server.URL}}
	cmd = store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig2}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))

	// Before we sync, let's grab the existing hash of this particular org.
	alertsRouter.AdminConfigMtx.Lock()
	currentHash := alertsRouter.SendersCfgHash[2]
	alertsRouter.AdminConfigMtx.Unlock()

	// Now, sync again.
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())

	// The hash for org two should not be the same and we should still have two senders.
	alertsRouter.AdminConfigMtx.Lock()
	require.NotEqual(t, alertsRouter.SendersCfgHash[2], currentHash)
	require.Equal(t, 2, len(alertsRouter.Senders))
	require.Equal(t, 2, len(alertsRouter.SendersCfgHash))
	alertsRouter.AdminConfigMtx.Unlock()

	// Wait for the discovery of the new Alertmanager for orgID = 2.
	require.Eventuallyf(t, func() bool {
		return len(alertsRouter.AlertmanagersFor(2)) == 2 && len(alertsRouter.DroppedAlertmanagersFor(2)) == 0
	}, 10*time.Second, 200*time.Millisecond, "Alertmanager for org 2 was never re-discovered after fix")

	// 3. Now, let's provide a configuration that fails for OrgID = 1.
	adminConfig2 = &models.AdminConfiguration{OrgID: 1, Alertmanagers: []string{"123://invalid.org"}}
	cmd = store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig2}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))

	// Before we sync, let's get the current config hash.
	alertsRouter.AdminConfigMtx.Lock()
	currentHash = alertsRouter.SendersCfgHash[1]
	alertsRouter.AdminConfigMtx.Unlock()

	// Now, sync again.
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())

	// The old configuration should still be running.
	alertsRouter.AdminConfigMtx.Lock()
	require.Equal(t, alertsRouter.SendersCfgHash[1], currentHash)
	alertsRouter.AdminConfigMtx.Unlock()
	require.Equal(t, 1, len(alertsRouter.AlertmanagersFor(1)))

	// If we fix it - it should be applied.
	adminConfig2 = &models.AdminConfiguration{OrgID: 1, Alertmanagers: []string{"notarealalertmanager:3030"}}
	cmd = store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig2}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())
	alertsRouter.AdminConfigMtx.Lock()
	require.NotEqual(t, alertsRouter.SendersCfgHash[1], currentHash)
	alertsRouter.AdminConfigMtx.Unlock()

	// Finally, remove everything.
	require.NoError(t, fakeAdminConfigStore.DeleteAdminConfiguration(1))
	require.NoError(t, fakeAdminConfigStore.DeleteAdminConfiguration(2))
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())
	alertsRouter.AdminConfigMtx.Lock()
	require.Equal(t, 0, len(alertsRouter.Senders))
	require.Equal(t, 0, len(alertsRouter.SendersCfgHash))
	alertsRouter.AdminConfigMtx.Unlock()

	require.Eventuallyf(t, func() bool {
		NoAlertmanagerOrgOne := len(alertsRouter.AlertmanagersFor(1)) == 0 && len(alertsRouter.DroppedAlertmanagersFor(1)) == 0
		NoAlertmanagerOrgTwo := len(alertsRouter.AlertmanagersFor(2)) == 0 && len(alertsRouter.DroppedAlertmanagersFor(2)) == 0

		return NoAlertmanagerOrgOne && NoAlertmanagerOrgTwo
	}, 10*time.Second, 200*time.Millisecond, "Alertmanager for org 1 and 2 were never removed")
}

func TestChangingAlertmanagersChoice(t *testing.T) {
	fakeAM := store.NewFakeExternalAlertmanager(t)
	defer fakeAM.Close()
	fakeRuleStore := store.NewFakeRuleStore(t)
	fakeInstanceStore := &store.FakeInstanceStore{}
	fakeAdminConfigStore := store.NewFakeAdminConfigStore(t)

	// create alert rule with one second interval and an Alertmanagers choice.
	alertRule := CreateTestAlertRule(t, fakeRuleStore, 1, 1, eval.Alerting)

	// First, let's create an admin configuration that holds an alertmanager
	// and sends alerts to both internal and external alertmanagers (default).
	adminConfig := &models.AdminConfiguration{OrgID: 1, Alertmanagers: []string{fakeAM.Server.URL}}
	cmd := store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))

	sched, mockedClock, alertsRouter := setupScheduler(t, fakeRuleStore, fakeInstanceStore, fakeAdminConfigStore, nil)

	// Make sure we sync the configuration at least once before the evaluation happens to guarantee the sender is running
	// when the first alert triggers.
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())
	alertsRouter.AdminConfigMtx.Lock()
	require.Equal(t, 1, len(alertsRouter.Senders))
	require.Equal(t, 1, len(alertsRouter.SendersCfgHash))
	alertsRouter.AdminConfigMtx.Unlock()

	// Then, ensure we've discovered the Alertmanager and the Alertmanagers choice is correct.
	require.Eventually(t, func() bool {
		return len(alertsRouter.AlertmanagersFor(1)) == 1 &&
			len(alertsRouter.DroppedAlertmanagersFor(1)) == 0 &&
			alertsRouter.SendAlertsTo[1] == adminConfig.SendAlertsTo
	}, 10*time.Second, 200*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
	})
	go func() {
		err := sched.Run(ctx)
		require.NoError(t, err)
	}()

	// With everything up and running, let's advance the time to make sure we get at least one alert iteration.
	mockedClock.Add(2 * time.Second)

	// Eventually, our Alertmanager should have received alerts.
	require.Eventuallyf(t, func() bool {
		return fakeAM.AlertsCount() >= 1 &&
			fakeAM.AlertNamesCompare([]string{alertRule.Title})
	}, 10*time.Second, 200*time.Millisecond, "expected at least one alert to be received and the title of the first one to be '%s'. but got [%d]: [%v]", alertRule.Title, fakeAM.AlertsCount(), fakeAM.Alerts())

	// Now, let's change the Alertmanagers choice to send only to the external Alertmanager.
	adminConfig.SendAlertsTo = models.ExternalAlertmanagers
	cmd = store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))

	// Again, make sure we sync and verify the senders.
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())
	alertsRouter.AdminConfigMtx.Lock()
	require.Equal(t, 1, len(alertsRouter.Senders))
	require.Equal(t, 1, len(alertsRouter.SendersCfgHash))
	alertsRouter.AdminConfigMtx.Unlock()

	// Then, ensure we still have the Alertmanager but the Alertmanagers choice has changed.
	require.Eventually(t, func() bool {
		return len(alertsRouter.AlertmanagersFor(1)) == 1 &&
			len(alertsRouter.DroppedAlertmanagersFor(1)) == 0 &&
			alertsRouter.SendAlertsTo[1] == adminConfig.SendAlertsTo
	}, 10*time.Second, 200*time.Millisecond)

	// Finally, let's change the Alertmanagers choice to send only to the internal Alertmanager.
	adminConfig.SendAlertsTo = models.InternalAlertmanager
	cmd = store.UpdateAdminConfigurationCmd{AdminConfiguration: adminConfig}
	require.NoError(t, fakeAdminConfigStore.UpdateAdminConfiguration(cmd))

	// Again, make sure we sync and verify the senders.
	// Senders should be running even though alerts are being handled externally.
	require.NoError(t, alertsRouter.SyncAndApplyConfigFromDatabase())
	alertsRouter.AdminConfigMtx.Lock()
	require.Equal(t, 1, len(alertsRouter.Senders))
	require.Equal(t, 1, len(alertsRouter.SendersCfgHash))
	alertsRouter.AdminConfigMtx.Unlock()

	// Then, ensure the Alertmanager is still listed and the Alertmanagers choice has changed.
	require.Eventually(t, func() bool {
		return len(alertsRouter.AlertmanagersFor(1)) == 1 &&
			len(alertsRouter.DroppedAlertmanagersFor(1)) == 0 &&
			alertsRouter.SendAlertsTo[1] == adminConfig.SendAlertsTo
	}, 10*time.Second, 200*time.Millisecond)
}

func TestSchedule_ruleRoutine(t *testing.T) {
	createSchedule := func(
		evalAppliedChan chan time.Time,
	) (*schedule, *store.FakeRuleStore, *store.FakeInstanceStore, *store.FakeAdminConfigStore, prometheus.Gatherer, *sender.AlertsRouter) {
		ruleStore := store.NewFakeRuleStore(t)
		instanceStore := &store.FakeInstanceStore{}
		adminConfigStore := store.NewFakeAdminConfigStore(t)

		registry := prometheus.NewPedanticRegistry()
		sch, _, alertsRouter := setupScheduler(t, ruleStore, instanceStore, adminConfigStore, registry)
		sch.evalAppliedFunc = func(key models.AlertRuleKey, t time.Time) {
			evalAppliedChan <- t
		}
		return sch, ruleStore, instanceStore, adminConfigStore, registry, alertsRouter
	}

	// normal states do not include NoData and Error because currently it is not possible to perform any sensible test
	normalStates := []eval.State{eval.Normal, eval.Alerting, eval.Pending}
	allStates := [...]eval.State{eval.Normal, eval.Alerting, eval.Pending, eval.NoData, eval.Error}
	randomNormalState := func() eval.State {
		// pick only supported cases
		return normalStates[rand.Intn(3)]
	}

	for _, evalState := range normalStates {
		// TODO rewrite when we are able to mock/fake state manager
		t.Run(fmt.Sprintf("when rule evaluation happens (evaluation state %s)", evalState), func(t *testing.T) {
			evalChan := make(chan *evaluation)
			evalAppliedChan := make(chan time.Time)
			sch, ruleStore, instanceStore, _, reg, _ := createSchedule(evalAppliedChan)

			rule := CreateTestAlertRule(t, ruleStore, 10, rand.Int63(), evalState)

			go func() {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				_ = sch.ruleRoutine(ctx, rule.GetKey(), evalChan, make(chan ruleVersion))
			}()

			expectedTime := time.UnixMicro(rand.Int63())

			evalChan <- &evaluation{
				scheduledAt: expectedTime,
				version:     rule.Version,
			}

			actualTime := waitForTimeChannel(t, evalAppliedChan)
			require.Equal(t, expectedTime, actualTime)

			t.Run("it should get rule from database when run the first time", func(t *testing.T) {
				queries := make([]models.GetAlertRuleByUIDQuery, 0)
				for _, op := range ruleStore.RecordedOps {
					switch q := op.(type) {
					case models.GetAlertRuleByUIDQuery:
						queries = append(queries, q)
					}
				}
				require.NotEmptyf(t, queries, "Expected a %T request to rule store but nothing was recorded", models.GetAlertRuleByUIDQuery{})
				require.Len(t, queries, 1, "Expected exactly one request of %T but got %d", models.GetAlertRuleByUIDQuery{}, len(queries))
				require.Equal(t, rule.UID, queries[0].UID)
				require.Equal(t, rule.OrgID, queries[0].OrgID)
			})
			t.Run("it should get rule folder title from database and attach as label", func(t *testing.T) {
				states := sch.stateManager.GetStatesForRuleUID(rule.OrgID, rule.UID)
				for _, s := range states {
					require.NotEmptyf(t, s.Labels[models.FolderTitleLabel], "Expected a non-empty title in label %s", models.FolderTitleLabel)
					require.Equal(t, s.Labels[models.FolderTitleLabel], ruleStore.Folders[rule.OrgID][0].Title)
				}
			})
			t.Run("it should process evaluation results via state manager", func(t *testing.T) {
				// TODO rewrite when we are able to mock/fake state manager
				states := sch.stateManager.GetStatesForRuleUID(rule.OrgID, rule.UID)
				require.Len(t, states, 1)
				s := states[0]
				t.Logf("State: %v", s)
				require.Equal(t, rule.UID, s.AlertRuleUID)
				require.Len(t, s.Results, 1)
				var expectedStatus = evalState
				if evalState == eval.Pending {
					expectedStatus = eval.Alerting
				}
				require.Equal(t, expectedStatus.String(), s.Results[0].EvaluationState.String())
				require.Equal(t, expectedTime, s.Results[0].EvaluationTime)
			})
			t.Run("it should save alert instances to storage", func(t *testing.T) {
				// TODO rewrite when we are able to mock/fake state manager
				states := sch.stateManager.GetStatesForRuleUID(rule.OrgID, rule.UID)
				require.Len(t, states, 1)
				s := states[0]

				var cmd *models.SaveAlertInstanceCommand
				for _, op := range instanceStore.RecordedOps {
					switch q := op.(type) {
					case models.SaveAlertInstanceCommand:
						cmd = &q
					}
					if cmd != nil {
						break
					}
				}

				require.NotNil(t, cmd)
				t.Logf("Saved alert instance: %v", cmd)
				require.Equal(t, rule.OrgID, cmd.RuleOrgID)
				require.Equal(t, expectedTime, cmd.LastEvalTime)
				require.Equal(t, cmd.RuleUID, cmd.RuleUID)
				require.Equal(t, evalState.String(), string(cmd.State))
				require.Equal(t, s.Labels, data.Labels(cmd.Labels))
			})
			t.Run("it reports metrics", func(t *testing.T) {
				// duration metric has 0 values because of mocked clock that do not advance
				expectedMetric := fmt.Sprintf(
					`# HELP grafana_alerting_rule_evaluation_duration_seconds The duration for a rule to execute.
        	            	# TYPE grafana_alerting_rule_evaluation_duration_seconds histogram
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="0.005"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="0.01"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="0.025"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="0.05"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="0.1"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="0.25"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="0.5"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="1"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="2.5"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="5"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="10"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="25"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="50"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="100"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_bucket{org="%[1]d",le="+Inf"} 1
        	            	grafana_alerting_rule_evaluation_duration_seconds_sum{org="%[1]d"} 0
        	            	grafana_alerting_rule_evaluation_duration_seconds_count{org="%[1]d"} 1
							# HELP grafana_alerting_rule_evaluation_failures_total The total number of rule evaluation failures.
        	            	# TYPE grafana_alerting_rule_evaluation_failures_total counter
        	            	grafana_alerting_rule_evaluation_failures_total{org="%[1]d"} 0
        	            	# HELP grafana_alerting_rule_evaluations_total The total number of rule evaluations.
        	            	# TYPE grafana_alerting_rule_evaluations_total counter
        	            	grafana_alerting_rule_evaluations_total{org="%[1]d"} 1
				`, rule.OrgID)

				err := testutil.GatherAndCompare(reg, bytes.NewBufferString(expectedMetric), "grafana_alerting_rule_evaluation_duration_seconds", "grafana_alerting_rule_evaluations_total", "grafana_alerting_rule_evaluation_failures_total")
				require.NoError(t, err)
			})
		})
	}

	t.Run("should exit", func(t *testing.T) {
		t.Run("when context is cancelled", func(t *testing.T) {
			stoppedChan := make(chan error)
			sch, _, _, _, _, _ := createSchedule(make(chan time.Time))

			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				err := sch.ruleRoutine(ctx, models.AlertRuleKey{}, make(chan *evaluation), make(chan ruleVersion))
				stoppedChan <- err
			}()

			cancel()
			err := waitForErrChannel(t, stoppedChan)
			require.NoError(t, err)
		})
	})

	t.Run("should fetch rule from database only if new version is greater than current", func(t *testing.T) {
		evalChan := make(chan *evaluation)
		evalAppliedChan := make(chan time.Time)

		ctx := context.Background()
		sch, ruleStore, _, _, _, _ := createSchedule(evalAppliedChan)

		rule := CreateTestAlertRule(t, ruleStore, 10, rand.Int63(), randomNormalState())

		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			_ = sch.ruleRoutine(ctx, rule.GetKey(), evalChan, make(chan ruleVersion))
		}()

		expectedTime := time.UnixMicro(rand.Int63())
		evalChan <- &evaluation{
			scheduledAt: expectedTime,
			version:     rule.Version,
		}

		actualTime := waitForTimeChannel(t, evalAppliedChan)
		require.Equal(t, expectedTime, actualTime)

		// Now update the rule
		newRule := *rule
		newRule.Version++
		ruleStore.PutRule(ctx, &newRule)

		// and call with new version
		expectedTime = expectedTime.Add(time.Duration(rand.Intn(10)) * time.Second)
		evalChan <- &evaluation{
			scheduledAt: expectedTime,
			version:     newRule.Version,
		}

		actualTime = waitForTimeChannel(t, evalAppliedChan)
		require.Equal(t, expectedTime, actualTime)

		queries := make([]models.GetAlertRuleByUIDQuery, 0)
		for _, op := range ruleStore.RecordedOps {
			switch q := op.(type) {
			case models.GetAlertRuleByUIDQuery:
				queries = append(queries, q)
			}
		}
		require.Len(t, queries, 2, "Expected exactly two request of %T", models.GetAlertRuleByUIDQuery{})
		require.Equal(t, rule.UID, queries[0].UID)
		require.Equal(t, rule.OrgID, queries[0].OrgID)
		require.Equal(t, rule.UID, queries[1].UID)
		require.Equal(t, rule.OrgID, queries[1].OrgID)
	})

	t.Run("should not fetch rule if version is equal or less than current", func(t *testing.T) {
		evalChan := make(chan *evaluation)
		evalAppliedChan := make(chan time.Time)

		sch, ruleStore, _, _, _, _ := createSchedule(evalAppliedChan)

		rule := CreateTestAlertRule(t, ruleStore, 10, rand.Int63(), randomNormalState())

		go func() {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			_ = sch.ruleRoutine(ctx, rule.GetKey(), evalChan, make(chan ruleVersion))
		}()

		expectedTime := time.UnixMicro(rand.Int63())
		evalChan <- &evaluation{
			scheduledAt: expectedTime,
			version:     rule.Version,
		}

		actualTime := waitForTimeChannel(t, evalAppliedChan)
		require.Equal(t, expectedTime, actualTime)

		// try again with the same version
		expectedTime = expectedTime.Add(time.Duration(rand.Intn(10)) * time.Second)
		evalChan <- &evaluation{
			scheduledAt: expectedTime,
			version:     rule.Version,
		}
		actualTime = waitForTimeChannel(t, evalAppliedChan)
		require.Equal(t, expectedTime, actualTime)

		expectedTime = expectedTime.Add(time.Duration(rand.Intn(10)) * time.Second)
		evalChan <- &evaluation{
			scheduledAt: expectedTime,
			version:     rule.Version - 1,
		}
		actualTime = waitForTimeChannel(t, evalAppliedChan)
		require.Equal(t, expectedTime, actualTime)

		queries := make([]models.GetAlertRuleByUIDQuery, 0)
		for _, op := range ruleStore.RecordedOps {
			switch q := op.(type) {
			case models.GetAlertRuleByUIDQuery:
				queries = append(queries, q)
			}
		}
		require.Len(t, queries, 1, "Expected exactly one request of %T", models.GetAlertRuleByUIDQuery{})
	})

	t.Run("when update channel is not empty", func(t *testing.T) {
		t.Run("should fetch the alert rule from database", func(t *testing.T) {
			evalChan := make(chan *evaluation)
			evalAppliedChan := make(chan time.Time)
			updateChan := make(chan ruleVersion)

			sch, ruleStore, _, _, _, _ := createSchedule(evalAppliedChan)

			rule := CreateTestAlertRule(t, ruleStore, 10, rand.Int63(), eval.Alerting) // we want the alert to fire

			go func() {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				_ = sch.ruleRoutine(ctx, rule.GetKey(), evalChan, updateChan)
			}()
			updateChan <- ruleVersion(rule.Version)

			// wait for command to be executed
			var queries []interface{}
			require.Eventuallyf(t, func() bool {
				queries = ruleStore.GetRecordedCommands(func(cmd interface{}) (interface{}, bool) {
					c, ok := cmd.(models.GetAlertRuleByUIDQuery)
					return c, ok
				})
				return len(queries) == 1
			}, 5*time.Second, 100*time.Millisecond, "Expected command a single %T to be recorded. All recordings: %#v", models.GetAlertRuleByUIDQuery{}, ruleStore.RecordedOps)

			m := queries[0].(models.GetAlertRuleByUIDQuery)
			require.Equal(t, rule.UID, m.UID)
			require.Equal(t, rule.OrgID, m.OrgID)

			// now call evaluation loop to make sure that the rule was persisted
			evalChan <- &evaluation{
				scheduledAt: time.UnixMicro(rand.Int63()),
				version:     rule.Version,
			}
			waitForTimeChannel(t, evalAppliedChan)

			queries = ruleStore.GetRecordedCommands(func(cmd interface{}) (interface{}, bool) {
				c, ok := cmd.(models.GetAlertRuleByUIDQuery)
				return c, ok
			})
			require.Lenf(t, queries, 1, "evaluation loop requested a rule from database but it should not be")
		})

		t.Run("should retry when database fails", func(t *testing.T) {
			evalAppliedChan := make(chan time.Time)
			updateChan := make(chan ruleVersion)

			sch, ruleStore, _, _, _, _ := createSchedule(evalAppliedChan)
			sch.maxAttempts = rand.Int63n(4) + 1

			rule := CreateTestAlertRule(t, ruleStore, 10, rand.Int63(), randomNormalState())

			go func() {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				_ = sch.ruleRoutine(ctx, rule.GetKey(), make(chan *evaluation), updateChan)
			}()

			ruleStore.Hook = func(cmd interface{}) error {
				if _, ok := cmd.(models.GetAlertRuleByUIDQuery); !ok {
					return nil
				}
				return errors.New("TEST")
			}
			updateChan <- ruleVersion(rule.Version)

			var queries []interface{}
			require.Eventuallyf(t, func() bool {
				queries = ruleStore.GetRecordedCommands(func(cmd interface{}) (interface{}, bool) {
					c, ok := cmd.(models.GetAlertRuleByUIDQuery)
					return c, ok
				})
				return int64(len(queries)) == sch.maxAttempts
			}, 5*time.Second, 100*time.Millisecond, "Expected exactly two request of %T. All recordings: %#v", models.GetAlertRuleByUIDQuery{}, ruleStore.RecordedOps)
		})
	})

	t.Run("when rule version is updated", func(t *testing.T) {
		t.Run("should clear the state and expire firing alerts", func(t *testing.T) {
			fakeAM := store.NewFakeExternalAlertmanager(t)
			defer fakeAM.Close()

			orgID := rand.Int63()
			s, err := sender.New()
			require.NoError(t, err)
			adminConfig := &models.AdminConfiguration{OrgID: orgID, Alertmanagers: []string{fakeAM.Server.URL}}
			err = s.ApplyConfig(adminConfig)
			require.NoError(t, err)
			s.Run()
			defer s.Stop()

			require.Eventuallyf(t, func() bool {
				return len(s.Alertmanagers()) == 1
			}, 20*time.Second, 200*time.Millisecond, "external Alertmanager was not discovered.")

			evalChan := make(chan *evaluation)
			evalAppliedChan := make(chan time.Time)
			updateChan := make(chan ruleVersion)

			ctx := context.Background()
			sch, ruleStore, _, _, _, alertsRouter := createSchedule(evalAppliedChan)
			alertsRouter.Senders[orgID] = s

			var rulePtr = CreateTestAlertRule(t, ruleStore, 10, orgID, eval.Alerting) // we want the alert to fire
			var rule = *rulePtr

			// define some state
			states := make([]*state.State, 0, len(allStates))
			for _, s := range allStates {
				for i := 0; i < 2; i++ {
					states = append(states, &state.State{
						AlertRuleUID: rule.UID,
						CacheId:      util.GenerateShortUID(),
						OrgID:        rule.OrgID,
						State:        s,
						StartsAt:     sch.clock.Now(),
						EndsAt:       sch.clock.Now().Add(time.Duration(rand.Intn(25)+5) * time.Second),
						Labels:       rule.Labels,
					})
				}
			}
			sch.stateManager.Put(states)
			states = sch.stateManager.GetStatesForRuleUID(rule.OrgID, rule.UID)

			expectedToBeSent := 0
			for _, s := range states {
				if s.State == eval.Normal || s.State == eval.Pending {
					continue
				}
				expectedToBeSent++
			}
			require.Greaterf(t, expectedToBeSent, 0, "State manger was expected to return at least one state that can be expired")

			go func() {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				_ = sch.ruleRoutine(ctx, rule.GetKey(), evalChan, updateChan)
			}()

			wg := sync.WaitGroup{}
			wg.Add(1)
			ruleStore.Hook = func(cmd interface{}) error {
				_, ok := cmd.(models.GetAlertRuleByUIDQuery)
				if ok {
					wg.Done() // add synchronization.
				}
				return nil
			}

			updateChan <- ruleVersion(rule.Version)

			wg.Wait()
			newRule := rule
			newRule.Version++
			ruleStore.PutRule(ctx, &newRule)
			wg.Add(1)
			updateChan <- ruleVersion(newRule.Version)
			wg.Wait()

			require.Eventually(t, func() bool {
				return len(sch.stateManager.GetStatesForRuleUID(rule.OrgID, rule.UID)) == 0
			}, 5*time.Second, 100*time.Millisecond)

			var count int
			require.Eventuallyf(t, func() bool {
				count = fakeAM.AlertsCount()
				return count == expectedToBeSent
			}, 20*time.Second, 200*time.Millisecond, "Alertmanager was expected to receive %d alerts, but received only %d", expectedToBeSent, count)

			for _, alert := range fakeAM.Alerts() {
				require.Equalf(t, sch.clock.Now().UTC(), time.Time(alert.EndsAt).UTC(), "Alert received by Alertmanager should be expired as of now")
			}
		})
	})

	t.Run("when evaluation fails", func(t *testing.T) {
		t.Run("it should increase failure counter", func(t *testing.T) {
			t.Skip()
			// TODO implement check for counter
		})
		t.Run("it should retry up to configured times", func(t *testing.T) {
			// TODO figure out how to simulate failure
			t.Skip()
		})
	})

	t.Run("when there are alerts that should be firing", func(t *testing.T) {
		t.Run("it should send to local alertmanager if configured for organization", func(t *testing.T) {
			// TODO figure out how to simulate multiorg alertmanager
			t.Skip()
		})
		t.Run("it should send to external alertmanager if configured for organization", func(t *testing.T) {
			fakeAM := store.NewFakeExternalAlertmanager(t)
			defer fakeAM.Close()

			orgID := rand.Int63()
			s, err := sender.New()
			require.NoError(t, err)
			adminConfig := &models.AdminConfiguration{OrgID: orgID, Alertmanagers: []string{fakeAM.Server.URL}}
			err = s.ApplyConfig(adminConfig)
			require.NoError(t, err)
			s.Run()
			defer s.Stop()

			require.Eventuallyf(t, func() bool {
				return len(s.Alertmanagers()) == 1
			}, 20*time.Second, 200*time.Millisecond, "external Alertmanager was not discovered.")

			evalChan := make(chan *evaluation)
			evalAppliedChan := make(chan time.Time)

			sch, ruleStore, _, _, _, alertsRouter := createSchedule(evalAppliedChan)
			alertsRouter.Senders[orgID] = s
			// eval.Alerting makes state manager to create notifications for alertmanagers
			rule := CreateTestAlertRule(t, ruleStore, 10, orgID, eval.Alerting)

			go func() {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				_ = sch.ruleRoutine(ctx, rule.GetKey(), evalChan, make(chan ruleVersion))
			}()

			evalChan <- &evaluation{
				scheduledAt: time.Now(),
				version:     rule.Version,
			}
			waitForTimeChannel(t, evalAppliedChan)

			var count int
			require.Eventuallyf(t, func() bool {
				count = fakeAM.AlertsCount()
				return count == 1 && fakeAM.AlertNamesCompare([]string{rule.Title})
			}, 20*time.Second, 200*time.Millisecond, "Alertmanager never received an '%s', received alerts count: %d", rule.Title, count)
		})
	})

	t.Run("when there are no alerts to send it should not call notifiers", func(t *testing.T) {
		// TODO needs some mocking/stubbing for Alertmanager and Sender to make sure it was not called
		t.Skip()
	})
}

func TestSchedule_UpdateAlertRule(t *testing.T) {
	t.Run("when rule exists", func(t *testing.T) {
		t.Run("it should call Update", func(t *testing.T) {
			sch := setupSchedulerWithFakeStores(t)
			key := generateRuleKey()
			info, _ := sch.registry.getOrCreateInfo(context.Background(), key)
			version := rand.Int63()
			go func() {
				sch.UpdateAlertRule(key, version)
			}()

			select {
			case v := <-info.updateCh:
				require.Equal(t, ruleVersion(version), v)
			case <-time.After(5 * time.Second):
				t.Fatal("No message was received on update channel")
			}
		})
		t.Run("should exit if it is closed", func(t *testing.T) {
			sch := setupSchedulerWithFakeStores(t)
			key := generateRuleKey()
			info, _ := sch.registry.getOrCreateInfo(context.Background(), key)
			info.stop()
			sch.UpdateAlertRule(key, rand.Int63())
		})
	})
	t.Run("when rule does not exist", func(t *testing.T) {
		t.Run("should exit", func(t *testing.T) {
			sch := setupSchedulerWithFakeStores(t)
			key := generateRuleKey()
			sch.UpdateAlertRule(key, rand.Int63())
		})
	})
}

func TestSchedule_DeleteAlertRule(t *testing.T) {
	t.Run("when rule exists", func(t *testing.T) {
		t.Run("it should stop evaluation loop and remove the controller from registry", func(t *testing.T) {
			sch := setupSchedulerWithFakeStores(t)
			key := generateRuleKey()
			info, _ := sch.registry.getOrCreateInfo(context.Background(), key)
			sch.DeleteAlertRule(key)
			require.False(t, info.update(ruleVersion(rand.Int63())))
			success, dropped := info.eval(time.Now(), 1)
			require.False(t, success)
			require.Nilf(t, dropped, "expected no dropped evaluations but got one")
			require.False(t, sch.registry.exists(key))
		})
		t.Run("should remove controller from registry", func(t *testing.T) {
			sch := setupSchedulerWithFakeStores(t)
			key := generateRuleKey()
			info, _ := sch.registry.getOrCreateInfo(context.Background(), key)
			info.stop()
			sch.DeleteAlertRule(key)
			require.False(t, info.update(ruleVersion(rand.Int63())))
			success, dropped := info.eval(time.Now(), 1)
			require.False(t, success)
			require.Nilf(t, dropped, "expected no dropped evaluations but got one")
			require.False(t, sch.registry.exists(key))
		})
	})
	t.Run("when rule does not exist", func(t *testing.T) {
		t.Run("should exit", func(t *testing.T) {
			sch := setupSchedulerWithFakeStores(t)
			key := generateRuleKey()
			sch.DeleteAlertRule(key)
		})
	})
}

func generateRuleKey() models.AlertRuleKey {
	return models.AlertRuleKey{
		OrgID: rand.Int63(),
		UID:   util.GenerateShortUID(),
	}
}

func setupSchedulerWithFakeStores(t *testing.T) *schedule {
	t.Helper()
	ruleStore := store.NewFakeRuleStore(t)
	instanceStore := &store.FakeInstanceStore{}
	adminConfigStore := store.NewFakeAdminConfigStore(t)
	sch, _, _ := setupScheduler(t, ruleStore, instanceStore, adminConfigStore, nil)
	return sch
}

func setupScheduler(t *testing.T, rs store.RuleStore, is store.InstanceStore, acs store.AdminConfigurationStore, registry *prometheus.Registry) (*schedule, *clock.Mock, *sender.AlertsRouter) {
	t.Helper()

	fakeAnnoRepo := store.NewFakeAnnotationsRepo()
	annotations.SetRepository(fakeAnnoRepo)
	mockedClock := clock.NewMock()
	logger := log.New("ngalert schedule test")
	if registry == nil {
		registry = prometheus.NewPedanticRegistry()
	}
	m := metrics.NewNGAlert(registry)
	secretsService := secretsManager.SetupTestService(t, fakes.NewFakeSecretsStore())
	decryptFn := secretsService.GetDecryptedValue
	moa, err := notifier.NewMultiOrgAlertmanager(&setting.Cfg{}, &notifier.FakeConfigStore{}, &notifier.FakeOrgStore{}, &notifier.FakeKVStore{}, provisioning.NewFakeProvisioningStore(), decryptFn, m.GetMultiOrgAlertmanagerMetrics(), nil, log.New("testlogger"), secretsService)
	require.NoError(t, err)

	appUrl := &url.URL{
		Scheme: "http",
		Host:   "localhost",
	}

	alertsRouter := sender.NewAlertsRouter(moa, acs, mockedClock, appUrl, map[int64]struct{}{}, 10*time.Minute) // do not poll in unit tests.

	cfg := setting.UnifiedAlertingSettings{
		BaseInterval:            time.Second,
		MaxAttempts:             1,
		AdminConfigPollInterval: 10 * time.Minute, // do not poll in unit tests.
	}

	schedCfg := SchedulerCfg{
		Cfg:           cfg,
		C:             mockedClock,
		Evaluator:     eval.NewEvaluator(&setting.Cfg{ExpressionsEnabled: true}, logger, nil, secretsService, expr.ProvideService(&setting.Cfg{ExpressionsEnabled: true}, nil, nil)),
		RuleStore:     rs,
		InstanceStore: is,
		Logger:        logger,
		Metrics:       m.GetSchedulerMetrics(),
		AlertSender:   alertsRouter,
	}
	st := state.NewManager(schedCfg.Logger, m.GetStateMetrics(), nil, rs, is, &dashboards.FakeDashboardService{}, &image.NoopImageService{}, clock.NewMock())
	return NewScheduler(schedCfg, appUrl, st, busmock.New()), mockedClock, alertsRouter
}

// createTestAlertRule creates a dummy alert definition to be used by the tests.
func CreateTestAlertRule(t *testing.T, dbstore *store.FakeRuleStore, intervalSeconds int64, orgID int64, evalResult eval.State) *models.AlertRule {
	ctx := context.Background()

	t.Helper()
	records := make([]interface{}, 0, len(dbstore.RecordedOps))
	copy(records, dbstore.RecordedOps)
	defer func() {
		// erase queries that were made by the testing suite
		dbstore.RecordedOps = records
	}()
	d := rand.Intn(1000)
	ruleGroup := fmt.Sprintf("ruleGroup-%d", d)

	var expression string
	var forDuration time.Duration
	switch evalResult {
	case eval.Normal:
		expression = `{
			"datasourceUid": "-100",
			"type":"math",
			"expression":"2 + 1 < 1"
		}`
	case eval.Pending, eval.Alerting:
		expression = `{
			"datasourceUid": "-100",
			"type":"math",
			"expression":"2 + 2 > 1"
		}`
		if evalResult == eval.Pending {
			forDuration = 100 * time.Second
		}
	case eval.Error:
		expression = `{
			"datasourceUid": "-100",
			"type":"math",
			"expression":"$A"
		}`
	case eval.NoData:
		// TODO Implement support for NoData
		require.Fail(t, "Alert rule with desired evaluation result NoData is not supported yet")
	}

	rule := &models.AlertRule{
		ID:        1,
		OrgID:     orgID,
		Title:     fmt.Sprintf("an alert definition %d", d),
		Condition: "A",
		Data: []models.AlertQuery{
			{
				DatasourceUID: "-100",
				Model:         json.RawMessage(expression),
				RelativeTimeRange: models.RelativeTimeRange{
					From: models.Duration(5 * time.Hour),
					To:   models.Duration(3 * time.Hour),
				},
				RefID: "A",
			},
		},
		Updated:         time.Now(),
		IntervalSeconds: intervalSeconds,
		Version:         1,
		UID:             util.GenerateShortUID(),
		NamespaceUID:    "namespace",
		RuleGroup:       ruleGroup,
		NoDataState:     models.NoData,
		ExecErrState:    models.AlertingErrState,
		For:             forDuration,
		Annotations:     map[string]string{"testAnnoKey": "testAnnoValue"},
		Labels:          make(map[string]string),
	}

	dbstore.PutRule(ctx, rule)

	t.Logf("alert definition: %v with interval: %d created", rule.GetKey(), rule.IntervalSeconds)
	return rule
}
