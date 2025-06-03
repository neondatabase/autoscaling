package plugin

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/neondatabase/autoscaling/pkg/plugin/metrics"
)

// TestNormalizeScoreRandomization tests that the NormalizeScore function randomizes scores
func TestNormalizeScoreRandomization(t *testing.T) {
	// Create a prometheus registry for testing
	registry := prometheus.NewRegistry()

	// Create a metrics plugin using the exported BuildPluginMetrics function
	metricsPlugin := metrics.BuildPluginMetrics(registry, nil)

	// Create a minimal enforcer with just enough dependencies to not crash
	//nolint:exhaustruct // Only initializing fields needed for the test
	enforcer := &AutoscaleEnforcer{
		logger:  zap.NewNop(),
		state:   &PluginState{},
		metrics: &metricsPlugin.Framework,
	}

	// Create a test pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
	}

	// Create a list of node scores
	originalScores := framework.NodeScoreList{
		{Name: "node1", Score: 100},
		{Name: "node2", Score: 50},
		{Name: "node3", Score: 25},
		{Name: "node4", Score: 10},
		{Name: "node5", Score: 0}, // Special case: score of 0 should remain unchanged
	}

	// Make a copy of the original scores for comparison
	expectedScores := make(map[string]int64)
	for _, nodeScore := range originalScores {
		expectedScores[nodeScore.Name] = nodeScore.Score
	}

	// Create a copy of scores to pass to NormalizeScore
	testScores := make(framework.NodeScoreList, len(originalScores))
	copy(testScores, originalScores)

	// Call the actual NormalizeScore method
	status := enforcer.NormalizeScore(context.Background(), nil, pod, testScores)

	// Check that the function returned success
	if status != nil && !status.IsSuccess() {
		t.Errorf("NormalizeScore returned non-success status: %v", status)
	}

	// Verify that scores have been changed to random values within the expected range
	minScore := framework.MinNodeScore + 1
	changedCount := 0

	for i, nodeScore := range testScores {
		originalScore := expectedScores[nodeScore.Name]

		// For score 0, it should remain unchanged
		if originalScore == 0 {
			if nodeScore.Score != 0 {
				t.Errorf("Node %s with original score 0 was changed to %d, expected to remain 0",
					nodeScore.Name, nodeScore.Score)
			}
			continue
		}

		// For other scores, they should be randomized
		if nodeScore.Score == originalScore {
			t.Errorf("Node %s score was not changed, remained at %d", nodeScore.Name, nodeScore.Score)
		}

		// Check that the new score is within the expected range [minScore, originalScore]
		if nodeScore.Score < minScore || nodeScore.Score > originalScore {
			t.Errorf("Node %s score %d is outside expected range [%d, %d]",
				nodeScore.Name, nodeScore.Score, minScore, originalScore)
		}

		if nodeScore.Score != originalScores[i].Score {
			changedCount++
		}
	}

	// Ensure at least some scores were changed
	if changedCount == 0 {
		t.Error("No scores were changed by NormalizeScore")
	}
}

// TestMultipleNormalizeScoreRuns verifies that NormalizeScore produces different random values
// on multiple runs
func TestMultipleNormalizeScoreRuns(t *testing.T) {
	// Create a prometheus registry for testing
	registry := prometheus.NewRegistry()

	// Create a metrics plugin using the exported BuildPluginMetrics function
	metricsPlugin := metrics.BuildPluginMetrics(registry, nil)

	// Create a minimal enforcer with just enough dependencies to not crash
	//nolint:exhaustruct // Only initializing fields needed for the test
	enforcer := &AutoscaleEnforcer{
		logger:  zap.NewNop(),
		state:   &PluginState{},
		metrics: &metricsPlugin.Framework,
	}

	// Create a test pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
	}

	// Create a node score with a high value to increase randomization space
	originalScore := framework.NodeScoreList{
		{Name: "node1", Score: 100},
	}

	// Run NormalizeScore multiple times and collect results
	const runs = 10
	results := make([]int64, runs)

	for i := 0; i < runs; i++ {
		// Create a copy of the original score
		testScore := make(framework.NodeScoreList, 1)
		testScore[0] = originalScore[0]

		// Call NormalizeScore
		enforcer.NormalizeScore(context.Background(), nil, pod, testScore)

		// Store the result
		results[i] = testScore[0].Score
	}

	// Check that we got at least some different values
	uniqueValues := make(map[int64]bool)
	for _, score := range results {
		uniqueValues[score] = true
	}

	// We should have at least 2 different values from 10 runs
	// (technically randomness could produce the same value multiple times,
	// but it's extremely unlikely with a range of [1, 100])
	if len(uniqueValues) < 2 {
		t.Errorf("Expected multiple different random values, but got only: %v", results)
	}
}
