package kiro

import "testing"

func TestIsContextLengthError(t *testing.T) {
	if ok, msg := IsContextLengthError([]byte(`{"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)); !ok || msg == "" {
		t.Errorf("should detect context-length-exceeds, got ok=%v msg=%q", ok, msg)
	}
	if ok, _ := IsContextLengthError([]byte(`{"message":"Input is too long for this model"}`)); !ok {
		t.Error("should detect 'Input is too long'")
	}
	if ok, _ := IsContextLengthError([]byte(`{"message":"Too many requests","reason":null}`)); ok {
		t.Error("429 should NOT be a context-length error")
	}
	if ok, _ := IsContextLengthError([]byte(``)); ok {
		t.Error("empty body should not match")
	}
}
