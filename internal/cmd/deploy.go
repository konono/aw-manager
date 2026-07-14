package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"gopkg.in/yaml.v3"

	"github.com/konono/aw-manager/internal/k8s"
	"github.com/konono/aw-manager/internal/manifest"
)

// awProfileConfig is a minimal struct to parse .aw.yml secrets configuration.
type awProfileConfig struct {
	Profiles map[string]struct {
		Kubernetes *struct {
			Secrets *struct {
				Env   []string `yaml:"env"`
				Files []struct {
					Source    string `yaml:"source"`
					MountPath string `yaml:"mountPath"`
					Env       string `yaml:"env"`
				} `yaml:"files"`
			} `yaml:"secrets"`
		} `yaml:"kubernetes"`
	} `yaml:"profiles"`
}

// extractSecretsFromAwConfig parses .aw.yml and extracts env vars and secret files
// for the specified profile's kubernetes.secrets config.
func (d *DeployCmd) extractSecretsFromAwConfig(logger *slog.Logger) (map[string]string, []secretFileEntry, error) {
	if d.AwConfig == "" {
		return nil, nil, nil
	}

	data, err := os.ReadFile(d.AwConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("reading aw config: %w", err)
	}

	var cfg awProfileConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("parsing aw config: %w", err)
	}

	profile, ok := cfg.Profiles[d.AwProfile]
	if !ok {
		logger.Warn("profile not found in .aw.yml, skipping secrets extraction", "profile", d.AwProfile)
		return nil, nil, nil
	}

	if profile.Kubernetes == nil || profile.Kubernetes.Secrets == nil {
		return nil, nil, nil
	}

	secrets := profile.Kubernetes.Secrets

	envVars := make(map[string]string)
	for _, entry := range secrets.Env {
		key, val, hasValue := strings.Cut(entry, "=")
		if !hasValue {
			val = os.Getenv(key)
		}
		if val == "" {
			logger.Warn("env var from .aw.yml not set on host, skipping", "var", key)
			continue
		}
		envVars[key] = val
		logger.Info("auto-detected env var from .aw.yml", "var", key)
	}

	var files []secretFileEntry
	homeDir, _ := os.UserHomeDir()
	podHome := d.Env["HOME"]
	if podHome == "" {
		podHome = "/home/agent"
	}
	for _, f := range secrets.Files {
		src := f.Source
		if strings.HasPrefix(src, "~/") {
			src = filepath.Join(homeDir, src[2:])
		}
		if _, err := os.Stat(src); err != nil {
			logger.Warn("secret file from .aw.yml not found on host, skipping", "source", f.Source)
			continue
		}
		// If mountPath starts with ~/, expand it relative to pod HOME.
		// Otherwise use the explicitly specified mountPath as-is.
		mountPath := f.MountPath
		if strings.HasPrefix(mountPath, "~") {
			mountPath = filepath.Join(podHome, mountPath[2:])
		} else if mountPath == f.MountPath && strings.HasPrefix(f.Source, "~/") {
			// For aw-manager pod: also mount at $HOME-relative path
			// so aw manifest can discover it via ~/
			mountPath = filepath.Join(podHome, f.Source[2:])
		}
		files = append(files, secretFileEntry{
			src:       src,
			mountPath: mountPath,
			envVar:    f.Env,
			key:       secretKey(src),
		})
		logger.Info("auto-detected secret file from .aw.yml", "source", f.Source, "mountPath", mountPath)
	}

	return envVars, files, nil
}

// Run deploys aw-manager and its dependencies to Kubernetes.
func (d *DeployCmd) Run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := d.validate(); err != nil {
		return err
	}

	restConfig, err := k8s.BuildRestConfig()
	if err != nil {
		return err
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	discoClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating discovery client: %w", err)
	}

	// Extract secrets from .aw.yml
	autoEnv, autoFiles, err := d.extractSecretsFromAwConfig(logger)
	if err != nil {
		return err
	}

	// Merge: manual --env overrides auto-detected
	if d.Env == nil {
		d.Env = make(map[string]string)
	}
	for k, v := range autoEnv {
		if _, exists := d.Env[k]; !exists {
			d.Env[k] = v
		}
	}

	ctx := context.Background()

	// Create namespaces directly — Apply() skips Namespace objects because
	// aw manifest output also contains them and the agent SA lacks cluster-level perms.
	// The deploy command runs with the user's kubeconfig which has namespace create perms.
	for _, ns := range []*unstructured.Unstructured{d.namespace(), d.awNamespace()} {
		nsGVR := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
		nsData, _ := json.Marshal(ns)
		_, err := dynClient.Resource(nsGVR).Patch(ctx, ns.GetName(), types.ApplyPatchType, nsData, metav1.PatchOptions{
			FieldManager: "aw-manager",
		})
		if err != nil {
			logger.Warn("namespace creation skipped (may already exist)", "namespace", ns.GetName(), "error", err)
		} else {
			logger.Info("namespace ready", "namespace", ns.GetName())
		}
	}

	var objects []*unstructured.Unstructured

	objects = append(objects, d.serviceAccount())
	objects = append(objects, d.role())
	objects = append(objects, d.roleBinding())

	if d.RedisURL == "" {
		logger.Info("deploying redis")
		objects = append(objects, d.redisDeployment())
		objects = append(objects, d.redisService())
	}

	if d.AwConfig != "" {
		awConfigData, err := os.ReadFile(d.AwConfig)
		if err != nil {
			return fmt.Errorf("reading aw config %s: %w", d.AwConfig, err)
		}
		objects = append(objects, d.awConfigMap(string(awConfigData)))
		logger.Info("mounting aw config", "path", d.AwConfig)
	}

	// Merge secret files: manual --secret-file + auto-detected from .aw.yml
	secretFiles, err := d.parseSecretFiles()
	if err != nil {
		return err
	}
	// Add auto-detected files that don't conflict with manual ones
	manualKeys := make(map[string]bool)
	for _, e := range secretFiles {
		manualKeys[e.key] = true
	}
	for _, e := range autoFiles {
		if !manualKeys[e.key] {
			secretFiles = append(secretFiles, e)
		}
	}

	if len(secretFiles) > 0 {
		fileSecret, err := d.fileSecret(secretFiles)
		if err != nil {
			return err
		}
		objects = append(objects, fileSecret)
		logger.Info("mounting secret files", "count", len(secretFiles))
	}

	objects = append(objects, d.secret())
	objects = append(objects, d.deployment(secretFiles))

	skipFn := func(kind, name string) {
		logger.Warn("skipping oversized resource", "kind", kind, "name", name)
	}
	if err := manifest.Apply(ctx, dynClient, discoClient, objects, skipFn); err != nil {
		return fmt.Errorf("applying manifests: %w", err)
	}

	logger.Info("deployment complete",
		"namespace", d.Namespace,
		"adapter", d.Adapter,
		"image", d.Image,
	)
	return nil
}

func (d *DeployCmd) validate() error {
	switch d.Adapter {
	case "slack":
		if d.SlackBotToken == "" || d.SlackAppToken == "" {
			return fmt.Errorf("--slack-bot-token and --slack-app-token are required for slack adapter")
		}
	case "discord":
		if d.DiscordToken == "" {
			return fmt.Errorf("--discord-token is required for discord adapter")
		}
	}
	return nil
}

func (d *DeployCmd) labels() map[string]interface{} {
	return map[string]interface{}{
		"app":                          "aw-manager",
		"app.kubernetes.io/name":       "aw-manager",
		"app.kubernetes.io/managed-by": "aw-manager",
	}
}

func (d *DeployCmd) namespace() *unstructured.Unstructured {
	return makeUnstructured(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name":   d.Namespace,
			"labels": d.labels(),
		},
	})
}

func (d *DeployCmd) awNamespace() *unstructured.Unstructured {
	return makeUnstructured(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name": d.AwNamespace,
			"labels": map[string]interface{}{
				"app.kubernetes.io/managed-by": "aw-manager",
			},
		},
	})
}

func (d *DeployCmd) serviceAccount() *unstructured.Unstructured {
	return makeUnstructured(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]interface{}{
			"name":      "aw-manager",
			"namespace": d.Namespace,
			"labels":    d.labels(),
		},
	})
}

func (d *DeployCmd) role() *unstructured.Unstructured {
	return makeUnstructured(map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "Role",
		"metadata": map[string]interface{}{
			"name":      "aw-manager",
			"namespace": d.AwNamespace,
			"labels":    d.labels(),
		},
		"rules": []interface{}{
			map[string]interface{}{
				"apiGroups": []interface{}{""},
				"resources": []interface{}{"pods", "pods/exec"},
				"verbs":     []interface{}{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			map[string]interface{}{
				"apiGroups": []interface{}{"apps"},
				"resources": []interface{}{"deployments"},
				"verbs":     []interface{}{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			map[string]interface{}{
				"apiGroups": []interface{}{""},
				"resources": []interface{}{"configmaps", "secrets", "serviceaccounts", "persistentvolumeclaims"},
				"verbs":     []interface{}{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
		},
	})
}

func (d *DeployCmd) roleBinding() *unstructured.Unstructured {
	return makeUnstructured(map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata": map[string]interface{}{
			"name":      "aw-manager",
			"namespace": d.AwNamespace,
			"labels":    d.labels(),
		},
		"roleRef": map[string]interface{}{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "Role",
			"name":     "aw-manager",
		},
		"subjects": []interface{}{
			map[string]interface{}{
				"kind":      "ServiceAccount",
				"name":      "aw-manager",
				"namespace": d.Namespace,
			},
		},
	})
}

func (d *DeployCmd) redisDeployment() *unstructured.Unstructured {
	return makeUnstructured(map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "redis",
			"namespace": d.Namespace,
			"labels":    d.labels(),
		},
		"spec": map[string]interface{}{
			"replicas": int64(1),
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app": "redis"},
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{"app": "redis"},
				},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":    "redis",
							"image":   "redis:7-alpine",
							"command": []interface{}{"redis-server", "--save", "", "--appendonly", "no"},
							"ports": []interface{}{
								map[string]interface{}{"containerPort": int64(6379)},
							},
							"resources": map[string]interface{}{
								"requests": map[string]interface{}{"cpu": "50m", "memory": "64Mi"},
								"limits":   map[string]interface{}{"cpu": "200m", "memory": "128Mi"},
							},
						},
					},
				},
			},
		},
	})
}

func (d *DeployCmd) redisService() *unstructured.Unstructured {
	return makeUnstructured(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name":      "redis",
			"namespace": d.Namespace,
			"labels":    d.labels(),
		},
		"spec": map[string]interface{}{
			"selector": map[string]interface{}{"app": "redis"},
			"ports": []interface{}{
				map[string]interface{}{
					"port":       int64(6379),
					"targetPort": int64(6379),
				},
			},
		},
	})
}

func (d *DeployCmd) secret() *unstructured.Unstructured {
	data := map[string]interface{}{}
	switch d.Adapter {
	case "slack":
		data["SLACK_BOT_TOKEN"] = base64.StdEncoding.EncodeToString([]byte(d.SlackBotToken))
		data["SLACK_APP_TOKEN"] = base64.StdEncoding.EncodeToString([]byte(d.SlackAppToken))
	case "discord":
		data["DISCORD_TOKEN"] = base64.StdEncoding.EncodeToString([]byte(d.DiscordToken))
	}

	return makeUnstructured(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      "aw-manager-secrets",
			"namespace": d.Namespace,
			"labels":    d.labels(),
		},
		"type": "Opaque",
		"data": data,
	})
}

type secretFileEntry struct {
	src       string
	mountPath string
	envVar    string
	key       string
}

func secretKey(path string) string {
	h := sha256.Sum256([]byte(path))
	base := filepath.Base(path)
	return base + "-" + hex.EncodeToString(h[:4])
}

func (d *DeployCmd) parseSecretFiles() ([]secretFileEntry, error) {
	var entries []secretFileEntry
	for _, s := range d.SecretFiles {
		parts := strings.SplitN(s, ":", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("--secret-file requires format src:mountPath[:ENV_VAR], got %q", s)
		}
		e := secretFileEntry{
			src:       parts[0],
			mountPath: parts[1],
			key:       secretKey(parts[0]),
		}
		if len(parts) == 3 {
			e.envVar = parts[2]
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (d *DeployCmd) fileSecret(entries []secretFileEntry) (*unstructured.Unstructured, error) {
	data := map[string]interface{}{}
	for _, e := range entries {
		content, err := os.ReadFile(e.src)
		if err != nil {
			return nil, fmt.Errorf("reading secret file %s: %w", e.src, err)
		}
		data[e.key] = base64.StdEncoding.EncodeToString(content)
	}
	return makeUnstructured(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      "aw-manager-files",
			"namespace": d.Namespace,
			"labels":    d.labels(),
		},
		"type": "Opaque",
		"data": data,
	}), nil
}

func (d *DeployCmd) awConfigMap(data string) *unstructured.Unstructured {
	return makeUnstructured(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name":      "aw-manager-config",
			"namespace": d.Namespace,
			"labels":    d.labels(),
		},
		"data": map[string]interface{}{
			".aw.yml": data,
		},
	})
}

func (d *DeployCmd) deployment(secretFiles []secretFileEntry) *unstructured.Unstructured {
	redisURL := d.RedisURL
	if redisURL == "" {
		redisURL = "redis://redis:6379"
	}

	env := []interface{}{
		map[string]interface{}{"name": "CHAT_ADAPTER", "value": d.Adapter},
		map[string]interface{}{"name": "REDIS_URL", "value": redisURL},
		map[string]interface{}{"name": "AW_PROFILE", "value": d.AwProfile},
		map[string]interface{}{"name": "AW_NAMESPACE", "value": d.AwNamespace},
		map[string]interface{}{"name": "AW_TOOL", "value": d.AwTool},
		map[string]interface{}{"name": "IDLE_TIMEOUT", "value": d.IdleTimeout},
		map[string]interface{}{"name": "MAX_CONCURRENT", "value": strconv.Itoa(d.MaxConcurrent)},
	}

	if d.AwConfig != "" {
		env = append(env, map[string]interface{}{"name": "AW_CONFIG_DIR", "value": "/etc/aw"})
	}

	for k, v := range d.Env {
		env = append(env, map[string]interface{}{"name": k, "value": v})
	}

	switch d.Adapter {
	case "slack":
		env = append(env,
			map[string]interface{}{
				"name": "SLACK_BOT_TOKEN",
				"valueFrom": map[string]interface{}{
					"secretKeyRef": map[string]interface{}{
						"name": "aw-manager-secrets",
						"key":  "SLACK_BOT_TOKEN",
					},
				},
			},
			map[string]interface{}{
				"name": "SLACK_APP_TOKEN",
				"valueFrom": map[string]interface{}{
					"secretKeyRef": map[string]interface{}{
						"name": "aw-manager-secrets",
						"key":  "SLACK_APP_TOKEN",
					},
				},
			},
		)
	case "discord":
		env = append(env,
			map[string]interface{}{
				"name": "DISCORD_TOKEN",
				"valueFrom": map[string]interface{}{
					"secretKeyRef": map[string]interface{}{
						"name": "aw-manager-secrets",
						"key":  "DISCORD_TOKEN",
					},
				},
			},
		)
	}

	container := map[string]interface{}{
		"name":  "server",
		"image": d.Image,
		"ports": []interface{}{
			map[string]interface{}{
				"name":          "metrics",
				"containerPort": int64(9090),
			},
		},
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{"cpu": "100m", "memory": "128Mi"},
			"limits":   map[string]interface{}{"cpu": "500m", "memory": "256Mi"},
		},
		"livenessProbe": map[string]interface{}{
			"httpGet": map[string]interface{}{
				"path": "/healthz",
				"port": "metrics",
			},
			"initialDelaySeconds": int64(5),
			"periodSeconds":       int64(30),
		},
		"readinessProbe": map[string]interface{}{
			"httpGet": map[string]interface{}{
				"path": "/healthz",
				"port": "metrics",
			},
			"initialDelaySeconds": int64(3),
			"periodSeconds":       int64(10),
		},
	}

	podSpec := map[string]interface{}{
		"serviceAccountName": "aw-manager",
		"containers":         []interface{}{container},
	}

	var volumeMounts []interface{}
	var volumes []interface{}

	if d.AwConfig != "" {
		volumeMounts = append(volumeMounts, map[string]interface{}{
			"name":      "aw-config",
			"mountPath": "/etc/aw",
			"readOnly":  true,
		})
		volumes = append(volumes, map[string]interface{}{
			"name": "aw-config",
			"configMap": map[string]interface{}{
				"name": "aw-manager-config",
			},
		})
	}

	if len(secretFiles) > 0 {
		for _, e := range secretFiles {
			volumeMounts = append(volumeMounts, map[string]interface{}{
				"name":      "secret-files",
				"mountPath": e.mountPath,
				"subPath":   e.key,
				"readOnly":  true,
			})
			if e.envVar != "" {
				env = append(env, map[string]interface{}{"name": e.envVar, "value": e.mountPath})
			}
		}
		volumes = append(volumes, map[string]interface{}{
			"name": "secret-files",
			"secret": map[string]interface{}{
				"secretName": "aw-manager-files",
			},
		})
	}

	container["env"] = env
	if len(volumeMounts) > 0 {
		container["volumeMounts"] = volumeMounts
	}
	if len(volumes) > 0 {
		podSpec["volumes"] = volumes
	}

	return makeUnstructured(map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      "aw-manager",
			"namespace": d.Namespace,
			"labels":    d.labels(),
		},
		"spec": map[string]interface{}{
			"replicas": int64(1),
			"selector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app": "aw-manager"},
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": map[string]interface{}{"app": "aw-manager"},
				},
				"spec": podSpec,
			},
		},
	})
}

func makeUnstructured(obj map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetUnstructuredContent(obj)
	gvk := u.GroupVersionKind()
	u.SetGroupVersionKind(gvk)

	// Ensure managedFields is not set so server-side apply works cleanly.
	u.SetManagedFields(nil)
	u.SetCreationTimestamp(metav1.Time{})
	return u
}
