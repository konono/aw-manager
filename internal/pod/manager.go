package pod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/konono/aw-manager/internal/config"
	"github.com/konono/aw-manager/internal/k8s"
	"github.com/konono/aw-manager/internal/manifest"
	"github.com/konono/aw-manager/internal/metrics"
	"github.com/konono/aw-manager/internal/session"
)

const (
	podReadyTimeout = 120 * time.Second
	execTimeout     = 300 * time.Second
	healthTimeout   = 5 * time.Second
)

// Manager handles pod lifecycle: creation via aw manifest, health checks, exec, and idle cleanup.
type Manager struct {
	clientset   kubernetes.Interface
	dynClient   dynamic.Interface
	discoClient discovery.DiscoveryInterface
	restConfig  *rest.Config
	cfg         *config.Config
	sessions    *session.Store
	logger      *slog.Logger
	keyLocks    sync.Map // per-SessionKey mutex to prevent concurrent EnsurePod races
}

// LockKey returns a per-session mutex. Callers use this to serialize EnsurePod + ExecTool
// for the same session key, preventing concurrent AI tool exec on the same pod.
func (m *Manager) LockKey(key session.SessionKey) *sync.Mutex {
	v, _ := m.keyLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// NewManager creates a Manager with in-cluster or kubeconfig-based K8s clients.
func NewManager(cfg *config.Config, sessions *session.Store, logger *slog.Logger) (*Manager, error) {
	restConfig, err := k8s.BuildRestConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating k8s clientset: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	discoClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating discovery client: %w", err)
	}

	mgr := &Manager{
		clientset:   clientset,
		dynClient:   dynClient,
		discoClient: discoClient,
		restConfig:  restConfig,
		cfg:         cfg,
		sessions:    sessions,
		logger:      logger,
	}

	mgr.syncPodsActiveGauge()

	return mgr, nil
}

// syncPodsActiveGauge initializes the PodsActive gauge from existing K8s pods
// to prevent drift after process restarts. Uses the label that aw manifest applies
// to all generated resources (see internal/manifest/labels.go in the aw repo).
func (m *Manager) syncPodsActiveGauge() {
	pods, err := m.clientset.CoreV1().Pods(m.cfg.AwNamespace).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=aw"},
	)
	if err != nil {
		m.logger.Warn("failed to sync PodsActive gauge", "error", err)
		return
	}
	var count float64
	for _, pod := range pods.Items {
		if isPodUsable(&pod) {
			count++
		}
	}
	metrics.PodsActive.Set(count)
	m.logger.Info("synced PodsActive gauge", "count", count)
}

func (m *Manager) instanceSuffix(key session.SessionKey) string {
	h := sha256.Sum256([]byte(key.UserID + ":" + key.ChannelID))
	return hex.EncodeToString(h[:6])
}

func (m *Manager) instanceName(key session.SessionKey) string {
	return fmt.Sprintf("aw-%s-%s", m.cfg.AwProfile, m.instanceSuffix(key))
}

// EnsurePod returns a running pod name for the given session key, creating one if necessary.
// The second return value indicates whether an existing pod was reused (true) or a new one was created (false).
func (m *Manager) EnsurePod(ctx context.Context, key session.SessionKey) (string, bool, error) {
	namespace := m.cfg.AwNamespace

	sess, err := m.sessions.GetSession(ctx, key)
	if err != nil {
		return "", false, fmt.Errorf("checking session: %w", err)
	}

	if sess != nil {
		pod, err := m.clientset.CoreV1().Pods(namespace).Get(ctx, sess.PodName, metav1.GetOptions{})
		if err == nil && isPodUsable(pod) {
			m.logger.Info("reusing existing pod", "user", key.UserID, "channel", key.ChannelID, "pod", sess.PodName)
			if err := m.sessions.TouchSession(ctx, key); err != nil {
				m.logger.Warn("failed to touch session", "error", err)
			}
			return sess.PodName, true, nil
		}

		if err == nil && isPodUnhealthy(pod) {
			m.logger.Warn("deleting unhealthy instance", "user", key.UserID, "channel", key.ChannelID, "pod", sess.PodName)
			_ = m.deleteInstance(ctx, namespace, m.instanceName(key))
		}

		_ = m.sessions.DeleteSession(ctx, key)
	}

	instName := m.instanceName(key)

	podName, err := m.findPodByInstance(ctx, namespace, instName)
	if err != nil {
		return "", false, err
	}
	if podName != "" {
		m.logger.Info("found existing pod by label", "user", key.UserID, "channel", key.ChannelID, "pod", podName)
		newSess := &session.Session{
			Key:       key,
			PodName:   podName,
			Namespace: namespace,
		}
		if err := m.sessions.SetSession(ctx, newSess); err != nil {
			return "", false, fmt.Errorf("saving session: %w", err)
		}
		return podName, true, nil
	}

	if err := m.createInstance(ctx, key); err != nil {
		_ = m.deleteInstance(ctx, namespace, instName)
		return "", false, err
	}

	podName, err = m.waitForPodReady(ctx, namespace, instName)
	if err != nil {
		_ = m.deleteInstance(ctx, namespace, instName)
		return "", false, fmt.Errorf("waiting for pod ready: %w", err)
	}

	newSess := &session.Session{
		Key:       key,
		PodName:   podName,
		Namespace: namespace,
	}
	if err := m.sessions.SetSession(ctx, newSess); err != nil {
		return "", false, fmt.Errorf("saving session: %w", err)
	}

	m.logger.Info("created new instance", "user", key.UserID, "channel", key.ChannelID, "instance", instName, "pod", podName)
	m.syncPodsActiveGauge()
	return podName, false, nil
}

func (m *Manager) hasNonTerminalPod(ctx context.Context, namespace, instanceName string) (bool, error) {
	labelSelector := fmt.Sprintf("app.kubernetes.io/instance=%s", instanceName)
	pods, err := m.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) findPodByInstance(ctx context.Context, namespace, instanceName string) (string, error) {
	labelSelector := fmt.Sprintf("app.kubernetes.io/instance=%s", instanceName)
	pods, err := m.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("listing pods: %w", err)
	}

	for _, pod := range pods.Items {
		if isPodUsable(&pod) {
			return pod.Name, nil
		}
	}
	return "", nil
}

func (m *Manager) createInstance(ctx context.Context, key session.SessionKey) error {
	suffix := m.instanceSuffix(key)

	cmd := exec.CommandContext(ctx, m.cfg.AwBinary, "manifest", m.cfg.AwProfile, "--name", suffix)
	if m.cfg.AwConfigDir != "" {
		cmd.Dir = m.cfg.AwConfigDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	m.logger.Info("running aw manifest", "profile", m.cfg.AwProfile, "name", suffix)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("aw manifest failed: %w, stderr: %s", err, stderr.String())
	}

	objects, err := manifest.ParseMultiDocYAML(stdout.Bytes())
	if err != nil {
		return fmt.Errorf("parsing manifest output: %w", err)
	}

	skipFn := func(kind, name string) {
		m.logger.Warn("skipping oversized resource", "kind", kind, "name", name)
	}
	if err := manifest.Apply(ctx, m.dynClient, m.discoClient, objects, skipFn); err != nil {
		return fmt.Errorf("applying manifests: %w", err)
	}

	return nil
}

func (m *Manager) waitForPodReady(ctx context.Context, namespace, instanceName string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, podReadyTimeout)
	defer cancel()

	labelSelector := fmt.Sprintf("app.kubernetes.io/instance=%s", instanceName)

	for {
		pods, err := m.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return "", fmt.Errorf("listing pods: %w", err)
		}

		for _, pod := range pods.Items {
			if isPodUsable(&pod) {
				return pod.Name, nil
			}
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				return "", fmt.Errorf("pod entered terminal phase: %s", pod.Status.Phase)
			}
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out waiting for pod with label %s", labelSelector)
		case <-time.After(2 * time.Second):
		}
	}
}

// ExecCommand runs a command in the agent container and returns stdout.
func (m *Manager) ExecCommand(ctx context.Context, podName, namespace string, command []string) (string, error) {
	return m.execWithStdin(ctx, podName, namespace, command, nil)
}

// ExecTool runs a tool command with the message piped via stdin.
func (m *Manager) ExecTool(ctx context.Context, podName, namespace string, command []string, message string) (string, error) {
	m.logger.Info("exec tool", "pod", podName, "command", command)
	result, err := m.execWithStdin(ctx, podName, namespace, command, strings.NewReader(message))
	if err != nil {
		m.logger.Error("exec tool failed", "pod", podName, "error", err)
	} else {
		m.logger.Info("exec tool completed", "pod", podName, "responseLen", len(result))
	}
	return result, err
}

func (m *Manager) execWithStdin(ctx context.Context, podName, namespace string, command []string, stdin io.Reader) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	useStdin := stdin != nil
	req := m.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "agent",
			Command:   command,
			Stdin:     useStdin,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(m.restConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	streamOpts := remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	}
	err = executor.StreamWithContext(ctx, streamOpts)
	if err != nil {
		stderrStr := stderr.String()
		if stderrStr != "" {
			return "", fmt.Errorf("exec failed: %w, stderr: %s", err, stderrStr)
		}
		return "", fmt.Errorf("exec failed: %w", err)
	}

	return stdout.String(), nil
}

// IsHealthy returns true if the pod responds to a simple exec within the health timeout.
func (m *Manager) IsHealthy(ctx context.Context, podName, namespace string) bool {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	_, err := m.ExecCommand(ctx, podName, namespace, []string{"echo", "ok"})
	return err == nil
}

// DeleteInstance removes all K8s resources for the given session and clears the session store.
func (m *Manager) DeleteInstance(ctx context.Context, key session.SessionKey) error {
	sess, err := m.sessions.GetSession(ctx, key)
	if err != nil {
		return fmt.Errorf("getting session: %w", err)
	}
	if sess == nil {
		return nil
	}

	if err := m.deleteInstance(ctx, sess.Namespace, m.instanceName(key)); err != nil {
		return err
	}

	if err := m.sessions.DeleteSession(ctx, key); err != nil {
		return err
	}

	// Do not delete from keyLocks: callers may still hold the mutex (e.g. cmdClear).
	m.syncPodsActiveGauge()
	return nil
}

func (m *Manager) deleteInstance(ctx context.Context, namespace, instanceName string) error {
	m.logger.Info("deleting instance", "instance", instanceName, "namespace", namespace)
	return manifest.DeleteByInstanceLabel(ctx, m.dynClient, m.discoClient, namespace, instanceName)
}

// CleanupIdlePods deletes instances that have been idle longer than the configured timeout.
func (m *Manager) CleanupIdlePods(ctx context.Context) error {
	keys, err := m.sessions.ListSessionKeys(ctx)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	for _, key := range keys {
		lastActive, err := m.sessions.GetLastActive(ctx, key)
		if err != nil {
			m.logger.Warn("failed to get last active time", "user", key.UserID, "channel", key.ChannelID, "error", err)
			continue
		}

		if lastActive.IsZero() || time.Since(lastActive) > m.cfg.IdleTimeout {
			mu := m.LockKey(key)
			mu.Lock()
			// Re-check after acquiring lock — handler may have touched the session
			lastActive2, _ := m.sessions.GetLastActive(ctx, key)
			if !lastActive2.IsZero() && time.Since(lastActive2) <= m.cfg.IdleTimeout {
				mu.Unlock()
				continue
			}
			m.logger.Info("cleaning up idle instance", "user", key.UserID, "channel", key.ChannelID, "lastActive", lastActive)
			if err := m.DeleteInstance(ctx, key); err != nil {
				m.logger.Warn("failed to cleanup idle instance", "user", key.UserID, "channel", key.ChannelID, "error", err)
			}
			mu.Unlock()
		}
	}

	return nil
}

// CleanupOrphanPods finds K8s pods/deployments with aw labels that have no
// corresponding Redis session and deletes them. This handles the case where
// Redis is flushed/restarted and session keys are lost.
func (m *Manager) CleanupOrphanPods(ctx context.Context) {
	deployments, err := m.clientset.AppsV1().Deployments(m.cfg.AwNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=aw",
	})
	if err != nil {
		m.logger.Warn("failed to list deployments for orphan cleanup", "error", err)
		return
	}

	sessionKeys, err := m.sessions.ListSessionKeys(ctx)
	if err != nil {
		m.logger.Warn("skipping orphan cleanup: redis unavailable", "error", err)
		return
	}
	activeInstances := make(map[string]bool)
	for _, key := range sessionKeys {
		activeInstances[m.instanceName(key)] = true
	}

	for _, dep := range deployments.Items {
		instance := dep.Labels["app.kubernetes.io/instance"]
		if instance == "" || activeInstances[instance] {
			continue
		}
		age := time.Since(dep.CreationTimestamp.Time)
		if age < m.cfg.IdleTimeout {
			continue
		}
		// Re-check Redis before deleting — a handler may have created a session
		// between ListSessionKeys and now
		latestKeys, err := m.sessions.ListSessionKeys(ctx)
		if err != nil {
			m.logger.Warn("skipping orphan deletion: redis unavailable", "error", err)
			return
		}
		stillOrphan := true
		for _, k := range latestKeys {
			if m.instanceName(k) == instance {
				stillOrphan = false
				break
			}
		}
		if !stillOrphan {
			continue
		}

		// Final safety check: if any non-terminal pod exists for this instance
		// (Running, Pending, ContainerCreating, etc.), skip deletion
		if active, err := m.hasNonTerminalPod(ctx, m.cfg.AwNamespace, instance); err != nil {
			m.logger.Warn("skipping orphan deletion: pod check failed", "instance", instance, "error", err)
			continue
		} else if active {
			m.logger.Info("skipping orphan with active pod", "instance", instance)
			continue
		}

		m.logger.Info("cleaning up orphan deployment", "instance", instance, "age", age.Round(time.Second))
		if err := manifest.DeleteByInstanceLabel(ctx, m.dynClient, m.discoClient, m.cfg.AwNamespace, instance); err != nil {
			m.logger.Warn("failed to delete orphan", "instance", instance, "error", err)
		}
	}
}

// StartIdleCleanup runs a background loop that periodically removes idle instances.
func (m *Manager) StartIdleCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.CleanupIdlePods(ctx); err != nil {
				m.logger.Error("idle cleanup failed", "error", err)
			}
			m.CleanupOrphanPods(ctx)
			m.syncPodsActiveGauge()
		}
	}
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isPodUsable(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning && isPodReady(pod) && pod.DeletionTimestamp == nil
}

func isPodUnhealthy(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			reason := cs.State.Waiting.Reason
			if reason == "CrashLoopBackOff" || reason == "ImagePullBackOff" || reason == "ErrImagePull" {
				return true
			}
		}
	}
	return false
}

