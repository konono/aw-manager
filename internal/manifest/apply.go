package manifest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/apimachinery/pkg/types"

	"encoding/json"
)

// MaxConfigMapBytes is the K8s etcd size limit for a single resource.
const MaxConfigMapBytes = 1048576

// ParseMultiDocYAML splits multi-document YAML into unstructured K8s objects.
func ParseMultiDocYAML(data []byte) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured

	reader := yamlutil.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading YAML document: %w", err)
		}

		doc = bytes.TrimSpace(doc)
		if len(doc) == 0 {
			continue
		}

		jsonData, err := yamlutil.ToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("converting YAML to JSON: %w", err)
		}

		if string(jsonData) == "null" || len(jsonData) == 0 {
			continue
		}

		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(jsonData); err != nil {
			return nil, fmt.Errorf("unmarshaling JSON: %w", err)
		}

		if obj.GetKind() == "" {
			continue
		}

		objects = append(objects, obj)
	}

	return objects, nil
}

// Apply creates or updates each K8s object using server-side apply.
// ConfigMaps exceeding the etcd size limit are skipped with a log message.
func Apply(ctx context.Context, dynClient dynamic.Interface, disco discovery.DiscoveryInterface, objects []*unstructured.Unstructured, skipFn func(kind, name string)) error {
	for _, obj := range objects {
		gvk := obj.GroupVersionKind()

		if gvk.Kind == "Namespace" {
			continue
		}

		if gvk.Kind == "Deployment" {
			stripUnsupportedFields(obj)
		}

		data, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshaling object: %w", err)
		}

		if gvk.Kind == "ConfigMap" && len(data) > MaxConfigMapBytes {
			if skipFn != nil {
				skipFn(gvk.Kind, obj.GetName())
			}
			continue
		}

		resource, namespaced, err := resolveGVR(disco, gvk)
		if err != nil {
			return fmt.Errorf("resolving resource for %s: %w", gvk, err)
		}

		var dr dynamic.ResourceInterface
		if namespaced {
			dr = dynClient.Resource(resource).Namespace(obj.GetNamespace())
		} else {
			dr = dynClient.Resource(resource)
		}

		_, err = dr.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
			FieldManager: "aw-manager",
		})
		if err != nil {
			return fmt.Errorf("applying %s %s/%s: %w", gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
		}
	}

	return nil
}

// DeleteByInstanceLabel removes all resources matching the app.kubernetes.io/instance label.
func DeleteByInstanceLabel(ctx context.Context, dynClient dynamic.Interface, _ discovery.DiscoveryInterface, namespace, instanceName string) error {
	labelSelector := fmt.Sprintf("app.kubernetes.io/instance=%s", instanceName)

	resourceTypes := []schema.GroupVersionResource{
		{Group: "apps", Version: "v1", Resource: "deployments"},
		{Group: "", Version: "v1", Resource: "configmaps"},
		{Group: "", Version: "v1", Resource: "secrets"},
		{Group: "", Version: "v1", Resource: "serviceaccounts"},
		{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
	}

	var errs []string
	for _, gvr := range resourceTypes {
		list, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			errs = append(errs, fmt.Sprintf("list %s: %v", gvr.Resource, err))
			continue
		}

		for _, item := range list.Items {
			err := dynClient.Resource(gvr).Namespace(namespace).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
			if err != nil {
				errs = append(errs, fmt.Sprintf("delete %s/%s: %v", gvr.Resource, item.GetName(), err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("deletion errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// stripUnsupportedFields removes fields that some K8s distributions do not support.
func stripUnsupportedFields(obj *unstructured.Unstructured) {
	unstructured.RemoveNestedField(obj.Object,
		"spec", "template", "spec", "securityContext", "hostUsers")
}

func resolveGVR(disco discovery.DiscoveryInterface, gvk schema.GroupVersionKind) (schema.GroupVersionResource, bool, error) {
	resources, err := disco.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return schema.GroupVersionResource{}, false, fmt.Errorf("fetching API resources for %s: %w", gvk.GroupVersion(), err)
	}

	for _, r := range resources.APIResources {
		if r.Kind == gvk.Kind && !strings.Contains(r.Name, "/") {
			return schema.GroupVersionResource{
				Group:    gvk.Group,
				Version:  gvk.Version,
				Resource: r.Name,
			}, r.Namespaced, nil
		}
	}

	return schema.GroupVersionResource{}, false, fmt.Errorf("resource not found for %s", gvk)
}
