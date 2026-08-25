package eval

import (
	"context"
	"testing"
)

func TestBasicSuiteSuccessRate(t *testing.T) {
	path, err := DefaultSuite()
	if err != nil {
		t.Fatal(err)
	}
	rep, err := RunFile(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Cases) < 8 {
		t.Fatalf("suite too small: %d cases", len(rep.Cases))
	}
	if rep.SuccessRate < 1.0 {
		t.Fatalf("success rate %.2f below 1.0\n%s", rep.SuccessRate, rep.Table())
	}
}
