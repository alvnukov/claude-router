package codex

import "testing"

func TestTextStreamKeepsItemsAndPartsSeparate(t *testing.T) {
	var stream TextStream
	stream.Delta(0, 0, "first")
	stream.Delta(1, 0, "sec")
	parts := []TextPart{{Type: "output_text", Text: "second"}, {Type: "refusal", Refusal: " refused"}}
	got, err := stream.Done(1, parts)
	if err != nil || got != "ond refused" {
		t.Fatalf("second item suffix = %q, err = %v", got, err)
	}
	got, err = stream.Done(1, parts)
	if err != nil || got != "" {
		t.Fatalf("repeated completed item = %q, err = %v", got, err)
	}
	got, err = stream.Done(0, []TextPart{{Type: "output_text", Text: "first!"}})
	if err != nil || got != "!" {
		t.Fatalf("first item suffix = %q, err = %v", got, err)
	}
}

func TestTextStreamRejectsChangedFinalText(t *testing.T) {
	var stream TextStream
	stream.Delta(0, 0, "original")
	if _, err := stream.Done(0, []TextPart{{Type: "output_text", Text: "replacement"}}); err == nil {
		t.Fatal("conflicting completed text was accepted")
	}
}
