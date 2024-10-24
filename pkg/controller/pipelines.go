package controller

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"
	goGitlab "github.com/xanzy/go-gitlab"
	"golang.org/x/exp/slices"

	"github.com/mvisonneau/gitlab-ci-pipelines-exporter/pkg/schemas"
)

// PullRefMetrics ..
func (c *Controller) PullRefMetrics(ctx context.Context, ref schemas.Ref) error {
	// At scale, the scheduled ref may be behind the actual state being stored
	// to avoid issues, we refresh it from the store before manipulating it
	if err := c.Store.GetRef(ctx, &ref); err != nil {
		return err
	}

	logFields := log.Fields{
		"project-name": ref.Project.Name,
		"ref":          ref.Name,
		"ref-kind":     ref.Kind,
	}

	// We need a different syntax if the ref is a merge-request
	var refName string
	if ref.Kind == schemas.RefKindMergeRequest {
		refName = fmt.Sprintf("refs/merge-requests/%s/head", ref.Name)
	} else {
		refName = ref.Name
	}

	pipelines, _, err := c.Gitlab.GetProjectPipelines(ctx, ref.Project.Name, &goGitlab.ListProjectPipelinesOptions{
		ListOptions: goGitlab.ListOptions{
			PerPage: int(ref.Project.Pull.Pipeline.PerRef),
			Page:    1,
		},
		Ref: &refName,
	})
	if err != nil {
		return fmt.Errorf("error fetching project pipelines for %s: %v", ref.Project.Name, err)
	}

	if len(pipelines) == 0 {
		log.WithFields(logFields).Debug("could not find any pipeline for the ref")

		return nil
	}

	// Reverse result list to have `ref`'s `LatestPipeline` untouched (compared to
	// default behavior) after looping over list
	slices.Reverse(pipelines)

	for _, apiPipeline := range pipelines {
		err := c.ProcessPipelinesMetrics(ctx, ref, apiPipeline)
		if err != nil {
			log.WithFields(log.Fields{
				"pipeline": apiPipeline.ID,
				"error":    err,
			}).Error("processing pipeline metrics failed")
		}
	}

	return nil
}

func (c *Controller) ProcessPipelinesMetrics(ctx context.Context, ref schemas.Ref, apiPipeline *goGitlab.PipelineInfo) error {
	finishedStatusesList := []string{
		"success",
		"failed",
		"skipped",
		"cancelled",
	}

	pipeline, err := c.Gitlab.GetRefPipeline(ctx, ref, apiPipeline.ID)
	if err != nil {
		return err
	}

	// fetch pipeline variables
	if ref.Project.Pull.Pipeline.Variables.Enabled {
		if exists, _ := c.Store.PipelineVariablesExist(ctx, pipeline); !exists {
			variables, err := c.Gitlab.GetRefPipelineVariablesAsConcatenatedString(ctx, ref, pipeline)
			c.Store.SetPipelineVariables(ctx, pipeline, variables)
			pipeline.Variables = variables
			if err != nil {
				return err
			}
		} else {
			variables, _ := c.Store.GetPipelineVariables(ctx, pipeline)
			pipeline.Variables = variables
		}
	}

	idMetric := schemas.Metric{
		Kind:   schemas.MetricKindID,
		Labels: ref.DefaultLabelsValues(pipeline),
		Value:  float64(pipeline.ID),
	}

	// TODO this comparison is a mistake
	// we should compare the whole pipeline object (as it was before) instead of
	// just the ID since properties like the status are likely to change
	if c.Store.GetMetric(ctx, &idMetric); ref.LatestPipeline.ID == 0 || idMetric.Value != float64(pipeline.ID) {
		formerPipeline := ref.LatestPipeline
		ref.LatestPipeline = pipeline

		// Update the ref in the store
		if err = c.Store.SetRef(ctx, ref); err != nil {
			return err
		}

		labels := ref.DefaultLabelsValues()

		// If the metric does not exist yet, start with 0 instead of 1
		// this could cause some false positives in prometheus
		// when restarting the exporter otherwise
		runCount := schemas.Metric{
			Kind:   schemas.MetricKindRunCount,
			Labels: labels,
		}

		storeGetMetric(ctx, c.Store, &runCount)

		if formerPipeline.ID != 0 && formerPipeline.ID != ref.LatestPipeline.ID {
			runCount.Value++
		}

		storeSetMetric(ctx, c.Store, runCount)

		storeSetMetric(ctx, c.Store, schemas.Metric{
			Kind:   schemas.MetricKindCoverage,
			Labels: labels,
			Value:  pipeline.Coverage,
		})

		storeSetMetric(ctx, c.Store, schemas.Metric{
			Kind:   schemas.MetricKindID,
			Labels: labels,
			Value:  float64(pipeline.ID),
		})

		emitStatusMetric(
			ctx,
			c.Store,
			schemas.MetricKindStatus,
			labels,
			statusesList[:],
			pipeline.Status,
			ref.Project.OutputSparseStatusMetrics,
		)

		storeSetMetric(ctx, c.Store, schemas.Metric{
			Kind:   schemas.MetricKindDurationSeconds,
			Labels: labels,
			Value:  pipeline.DurationSeconds,
		})

		storeSetMetric(ctx, c.Store, schemas.Metric{
			Kind:   schemas.MetricKindQueuedDurationSeconds,
			Labels: labels,
			Value:  pipeline.QueuedDurationSeconds,
		})

		storeSetMetric(ctx, c.Store, schemas.Metric{
			Kind:   schemas.MetricKindTimestamp,
			Labels: labels,
			Value:  pipeline.Timestamp,
		})

		if ref.Project.Pull.Pipeline.Jobs.Enabled {
			if err := c.PullRefPipelineJobsMetrics(ctx, ref); err != nil {
				return err
			}
		}
	} else {
		if err := c.PullRefMostRecentJobsMetrics(ctx, ref); err != nil {
			return err
		}
	}

	// fetch pipeline test report
	if ref.Project.Pull.Pipeline.TestReports.Enabled && slices.Contains(finishedStatusesList, ref.LatestPipeline.Status) {
		ref.LatestPipeline.TestReport, err = c.Gitlab.GetRefPipelineTestReport(ctx, ref)
		if err != nil {
			return err
		}

		c.ProcessTestReportMetrics(ctx, ref, ref.LatestPipeline.TestReport)

		for _, ts := range ref.LatestPipeline.TestReport.TestSuites {
			c.ProcessTestSuiteMetrics(ctx, ref, ts)
			// fetch pipeline test cases
			if ref.Project.Pull.Pipeline.TestReports.TestCases.Enabled {
				for _, tc := range ts.TestCases {
					c.ProcessTestCaseMetrics(ctx, ref, ts, tc)
				}
			}
		}
	}

	return nil
}

// ProcessTestReportMetrics ..
func (c *Controller) ProcessTestReportMetrics(ctx context.Context, ref schemas.Ref, tr schemas.TestReport) {
	testReportLogFields := log.Fields{
		"project-name": ref.Project.Name,
		"ref":          ref.Name,
	}

	labels := ref.DefaultLabelsValues()

	// Refresh ref state from the store
	if err := c.Store.GetRef(ctx, &ref); err != nil {
		log.WithContext(ctx).
			WithFields(testReportLogFields).
			WithError(err).
			Error("getting ref from the store")

		return
	}

	log.WithFields(testReportLogFields).Trace("processing test report metrics")

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestReportErrorCount,
		Labels: labels,
		Value:  float64(tr.ErrorCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestReportFailedCount,
		Labels: labels,
		Value:  float64(tr.FailedCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestReportSkippedCount,
		Labels: labels,
		Value:  float64(tr.SkippedCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestReportSuccessCount,
		Labels: labels,
		Value:  float64(tr.SuccessCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestReportTotalCount,
		Labels: labels,
		Value:  float64(tr.TotalCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestReportTotalTime,
		Labels: labels,
		Value:  float64(tr.TotalTime),
	})
}

// ProcessTestSuiteMetrics ..
func (c *Controller) ProcessTestSuiteMetrics(ctx context.Context, ref schemas.Ref, ts schemas.TestSuite) {
	testSuiteLogFields := log.Fields{
		"project-name":    ref.Project.Name,
		"ref":             ref.Name,
		"test-suite-name": ts.Name,
	}

	labels := ref.DefaultLabelsValues()
	labels["test_suite_name"] = ts.Name

	// Refresh ref state from the store
	if err := c.Store.GetRef(ctx, &ref); err != nil {
		log.WithContext(ctx).
			WithFields(testSuiteLogFields).
			WithError(err).
			Error("getting ref from the store")

		return
	}

	log.WithFields(testSuiteLogFields).Trace("processing test suite metrics")

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestSuiteErrorCount,
		Labels: labels,
		Value:  float64(ts.ErrorCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestSuiteFailedCount,
		Labels: labels,
		Value:  float64(ts.FailedCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestSuiteSkippedCount,
		Labels: labels,
		Value:  float64(ts.SkippedCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestSuiteSuccessCount,
		Labels: labels,
		Value:  float64(ts.SuccessCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestSuiteTotalCount,
		Labels: labels,
		Value:  float64(ts.TotalCount),
	})

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestSuiteTotalTime,
		Labels: labels,
		Value:  ts.TotalTime,
	})
}

func (c *Controller) ProcessTestCaseMetrics(ctx context.Context, ref schemas.Ref, ts schemas.TestSuite, tc schemas.TestCase) {
	testCaseLogFields := log.Fields{
		"project-name":     ref.Project.Name,
		"ref":              ref.Name,
		"test-suite-name":  ts.Name,
		"test-case-name":   tc.Name,
		"test-case-status": tc.Status,
	}

	labels := ref.DefaultLabelsValues()
	labels["test_suite_name"] = ts.Name
	labels["test_case_name"] = tc.Name
	labels["test_case_classname"] = tc.Classname

	// Get the existing ref from the store
	if err := c.Store.GetRef(ctx, &ref); err != nil {
		log.WithContext(ctx).
			WithFields(testCaseLogFields).
			WithError(err).
			Error("getting ref from the store")

		return
	}

	log.WithFields(testCaseLogFields).Trace("processing test case metrics")

	storeSetMetric(ctx, c.Store, schemas.Metric{
		Kind:   schemas.MetricKindTestCaseExecutionTime,
		Labels: labels,
		Value:  tc.ExecutionTime,
	})

	emitStatusMetric(
		ctx,
		c.Store,
		schemas.MetricKindTestCaseStatus,
		labels,
		statusesList[:],
		tc.Status,
		ref.Project.OutputSparseStatusMetrics,
	)
}
