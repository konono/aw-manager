package manifest

import (
	"testing"
)

func TestParseMultiDocYAML_Basic(t *testing.T) {
	yaml := `apiVersion: v1
kind: Namespace
metadata:
  name: test
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  namespace: test
data:
  key: value
`
	objects, err := ParseMultiDocYAML([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(objects))
	}
	if objects[0].GetKind() != "Namespace" {
		t.Errorf("expected Namespace, got %s", objects[0].GetKind())
	}
	if objects[1].GetKind() != "ConfigMap" {
		t.Errorf("expected ConfigMap, got %s", objects[1].GetKind())
	}
	if objects[1].GetName() != "test-cm" {
		t.Errorf("expected test-cm, got %s", objects[1].GetName())
	}
}

func TestParseMultiDocYAML_Empty(t *testing.T) {
	objects, err := ParseMultiDocYAML([]byte(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objects) != 0 {
		t.Errorf("expected 0 objects, got %d", len(objects))
	}
}

func TestParseMultiDocYAML_SkipsEmptyDocs(t *testing.T) {
	yaml := `---
---
apiVersion: v1
kind: Namespace
metadata:
  name: test
---
`
	objects, err := ParseMultiDocYAML([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objects) != 1 {
		t.Errorf("expected 1 object, got %d", len(objects))
	}
}
