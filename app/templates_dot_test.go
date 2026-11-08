package main

import "testing"

func TestDottedInput(t *testing.T) {
	g, err := parseWorkflow([]byte(`{"92":{"class_type":"SaveVideo","inputs":{"format.codec":"h264"}},"140:3":{"class_type":"X","inputs":{"seed":1}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := setInput(g, "92.format.codec", "vp9"); err != nil {
		t.Fatal(err)
	}
	if g["92"]["inputs"].(map[string]any)["format.codec"] != "vp9" {
		t.Fatal("not set")
	}
	if err := setInput(g, "140:3.seed", 5.0); err != nil {
		t.Fatal(err)
	}
}
