package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// A batch item that carries no `schema` used to poison every child in the
// batch. executeBatch re-marshals the item into a taskArgs, and a nil
// json.RawMessage marshals as the literal `null`; the child's yield tool then
// advertised `{"result": null}` as its result schema, and the provider rejected
// the whole child request at stream start:
//
//	HTTP 400 ... /properties/result: null is not of types "boolean","object"
//
// So the failure was not "the batch lost a schema" — it was "no child in the
// batch could start at all", including the ones whose items were fine.
func TestBatchItemWithoutSchemaDoesNotPoisonTheYieldSchema(t *testing.T) {
	sub, err := json.Marshal(taskArgs{Prompt: "p", Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	// Premise: the round trip still produces the literal null. If this stops
	// being true the bug is fixed elsewhere and this test should move.
	if !strings.Contains(string(sub), `"schema":null`) {
		t.Fatalf("premise gone: a batch item no longer marshals schema:null: %s", sub)
	}
	var a taskArgs
	if err := json.Unmarshal(sub, &a); err != nil {
		t.Fatal(err)
	}
	yt := &yieldTool{schema: (&SubagentOutput{Schema: a.Schema, Strict: a.Strict}).schema()}
	if p := string(yt.Parameters()); strings.Contains(p, `"result":null`) {
		t.Fatalf("yield advertises a null result schema: %s", p)
	}
	if !strings.Contains(string(yt.Parameters()), `"result":{"type":"string"`) {
		t.Fatalf("yield should fall back to the free-form string result: %s", yt.Parameters())
	}
}

// A schema that IS present still wins — the guard is about the null literal,
// not about disabling the feature.
func TestSubagentOutputSchemaKeepsARealSchema(t *testing.T) {
	sch := json.RawMessage(`{"type":"object","properties":{"n":{"type":"number"}}}`)
	o := &SubagentOutput{Schema: sch}
	if got := string(o.schema()); got != string(sch) {
		t.Fatalf("schema() = %s, want the configured schema", got)
	}
}
