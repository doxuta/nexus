package services

// draft_class_test.go — specs/063-draft-publish T012a (FR-070): the class table and the adapter
// registry describe the same set. Every slice that adds a draftable type extends BOTH in one commit;
// this test is what makes forgetting one of them a red build instead of a wrong promise to the builder.

import (
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/metaversion"
)

func productionTypes() *metaversion.Registry {
	return NewMetaRegistry() // the registry cmd/server wires
}

func TestClassOf_AgreesWithRegistry(t *testing.T) {
	reg := productionTypes()
	for _, typ := range metaversion.TypesOfKind(metaversion.Draft) {
		if _, ok := reg.Get(typ); !ok {
			t.Errorf("type %q is classed Draft but has no adapter in the registry: the UI would promise a draft that cannot be published", typ)
		}
	}
	for _, typ := range reg.Types() {
		if got := metaversion.ClassOf(typ).Kind; got != metaversion.Draft {
			t.Errorf("registry type %q is classed %q, want draft", typ, got)
		}
	}
	for _, typ := range metaversion.TypesOfKind(metaversion.DirectTraced) {
		if _, ok := reg.Get(typ); ok {
			t.Errorf("type %q has a draft adapter but is classed direct_traced", typ)
		}
	}
}

// The webhook key material never leaves through the changes / versions APIs.
func TestWorkflowDocAdapter_RedactMasksWebhookKey(t *testing.T) {
	doc := json.RawMessage(`{"name":"hook","entityName":"deals","trigger":"after_update","condition":null,` +
		`"actions":[{"type":"outbound_webhook","url":"https://partner.example/h","secret":"S3CRET-VALUE","headers":{"Authorization":"Bearer TOPSECRET"}}],` +
		`"isActive":true,"intervalSeconds":0}`)
	out := string(NewWorkflowDocAdapter().Redact(doc))
	for _, leaked := range []string{"S3CRET-VALUE", "TOPSECRET"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("Redact leaked %q: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "hook") || !strings.Contains(out, "partner.example") {
		t.Fatalf("Redact dropped the non-secret content: %s", out)
	}
	if got := string(NewWorkflowDocAdapter().Redact(json.RawMessage(`{broken`))); got != "null" {
		t.Fatalf("an unreadable body must not be echoed, got %s", got)
	}
}

func TestWorkflowDocAdapter_Entities(t *testing.T) {
	a := NewWorkflowDocAdapter()
	if got := a.Entities(json.RawMessage(`{"entityName":"deals"}`)); len(got) != 1 || got[0] != "deals" {
		t.Fatalf("Entities = %v", got)
	}
	if got := a.Entities(nil); got != nil {
		t.Fatalf("Entities(nil) = %v", got)
	}
}
