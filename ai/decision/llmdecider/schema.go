package llmdecider

// decisionSchema is the JSON Schema for decision.Decision sent as
// ai.ChatRequest.ResponseSchema. It is kept as a static, tested constant
// (rather than reflected from the struct) so a schema change is a visible,
// reviewed diff and adapters that need native structured-output support
// (e.g. strict JSON Schema subsets) can be checked against it directly.
//
// Keep this in sync with ai/decision.Decision; TestDecisionSchema_MatchesFields
// in schema_test.go checks the field names line up.
const decisionSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "module": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "value": {"type": "string"},
        "confidence": {"type": "number", "minimum": 0, "maximum": 1}
      },
      "required": ["value", "confidence"]
    },
    "intent": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "value": {"type": "string"},
        "confidence": {"type": "number", "minimum": 0, "maximum": 1}
      },
      "required": ["value", "confidence"]
    },
    "interaction": {
      "type": "string",
      "enum": ["command", "question", "confirmation", "rejection", "correction", "continuation", "cancellation", "undo", "chat"]
    },
    "reference": {
      "type": ["object", "null"],
      "additionalProperties": false,
      "properties": {
        "kind": {"type": "string"},
        "expression": {"type": "string"},
        "pronoun": {"type": "boolean"}
      },
      "required": ["kind"]
    },
    "requiredScopes": {"type": "array", "items": {"type": "string"}},
    "requiredData": {"type": "array", "items": {"type": "string"}},
    "slots": {"type": "object", "additionalProperties": {"type": "string"}},
    "canHandleDeterministically": {"type": "boolean"},
    "needsLLM": {"type": "boolean"},
    "presentation": {"type": "string"}
  },
  "required": ["module", "intent", "interaction", "canHandleDeterministically", "needsLLM"]
}`
