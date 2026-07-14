package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/konono/aw-manager/internal/k8s"
)

// Run removes aw-manager and all agent resources from Kubernetes.
func (u *UndeployCmd) Run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	restConfig, err := k8s.BuildRestConfig()
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating k8s clientset: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	ctx := context.Background()

	// Delete agent resources in aw namespace
	logger.Info("cleaning up agent resources", "namespace", u.AwNamespace)
	if err := deleteAllByLabel(ctx, dynClient, u.AwNamespace, "app.kubernetes.io/managed-by=aw"); err != nil {
		logger.Warn("failed to clean agent resources", "error", err)
	}

	// Delete aw-manager resources in system namespace
	logger.Info("cleaning up aw-manager resources", "namespace", u.Namespace)
	if err := deleteAllByLabel(ctx, dynClient, u.Namespace, "app.kubernetes.io/managed-by=aw-manager"); err != nil {
		logger.Warn("failed to clean aw-manager resources", "error", err)
	}
	// Also delete by app label (Redis, secrets, configmaps)
	if err := deleteAllByLabel(ctx, dynClient, u.Namespace, "app=aw-manager"); err != nil {
		logger.Warn("failed to clean aw-manager app resources", "error", err)
	}
	if err := deleteAllByLabel(ctx, dynClient, u.Namespace, "app=redis"); err != nil {
		logger.Warn("failed to clean redis resources", "error", err)
	}

	// Delete namespaces if --all
	if u.All {
		for _, ns := range []string{u.Namespace, u.AwNamespace} {
			logger.Info("deleting namespace", "namespace", ns)
			if err := clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil {
				logger.Warn("failed to delete namespace", "namespace", ns, "error", err)
			}
		}
	}

	logger.Info("undeploy complete")
	return nil
}

func deleteAllByLabel(ctx context.Context, dynClient dynamic.Interface, namespace, labelSelector string) error {
	resourceTypes := []schema.GroupVersionResource{
		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "", Version: "v1", Resource: "services"},
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "", Version: "v1", Resource: "serviceaccounts"},
		{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
	}

	for _, gvr := range resourceTypes {
		list, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			continue
		}
		for _, item := range list.Items {
			_ = dynClient.Resource(gvr).Namespace(namespace).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
		}
	}
	return nil
}
