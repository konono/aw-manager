package pod

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/konono/aw-manager/internal/config"
	"github.com/konono/aw-manager/internal/session"
)

func TestIsPodUnhealthy_Running(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if isPodUnhealthy(pod) {
		t.Error("running pod should not be unhealthy")
	}
}

func TestIsPodUnhealthy_Failed(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}
	if !isPodUnhealthy(pod) {
		t.Error("failed pod should be unhealthy")
	}
}

func TestIsPodUnhealthy_CrashLoopBackOff(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			},
		},
	}
	if !isPodUnhealthy(pod) {
		t.Error("CrashLoopBackOff pod should be unhealthy")
	}
}

func TestIsPodReady_True(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	if !isPodReady(pod) {
		t.Error("pod with Ready=True should be ready")
	}
}

func TestIsPodReady_False(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}
	if isPodReady(pod) {
		t.Error("pod with Ready=False should not be ready")
	}
}

func TestIsPodReady_NoConditions(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{}}
	if isPodReady(pod) {
		t.Error("pod with no conditions should not be ready")
	}
}

func TestIsPodUsable_RunningReadyNotTerminating(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	if !isPodUsable(pod) {
		t.Error("running, ready, non-terminating pod should be usable")
	}
}

func TestIsPodUsable_Terminating(t *testing.T) {
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	if isPodUsable(pod) {
		t.Error("terminating pod (DeletionTimestamp set) should not be usable")
	}
}

func TestIsPodUsable_NotReady(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}
	if isPodUsable(pod) {
		t.Error("pod with Ready=False should not be usable")
	}
}

func TestIsPodUsable_PendingPhase(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if isPodUsable(pod) {
		t.Error("pending pod should not be usable")
	}
}

func TestInstanceSuffix_Deterministic(t *testing.T) {
	m := &Manager{cfg: &config.Config{AwProfile: "test"}}
	key := session.SessionKey{UserID: "U123", ChannelID: "C456"}
	s1 := m.instanceSuffix(key)
	s2 := m.instanceSuffix(key)
	if s1 != s2 {
		t.Errorf("instanceSuffix not deterministic: %s != %s", s1, s2)
	}
	if len(s1) != 12 {
		t.Errorf("expected 12 hex chars, got %d: %s", len(s1), s1)
	}
}

func TestInstanceSuffix_DifferentKeys(t *testing.T) {
	m := &Manager{cfg: &config.Config{AwProfile: "test"}}
	s1 := m.instanceSuffix(session.SessionKey{UserID: "U1", ChannelID: "C1"})
	s2 := m.instanceSuffix(session.SessionKey{UserID: "U1", ChannelID: "C2"})
	if s1 == s2 {
		t.Error("different keys should produce different suffixes")
	}
}
