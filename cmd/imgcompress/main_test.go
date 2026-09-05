package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestExistingOutputIsPreserved(t *testing.T) {
	for _, samePath := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing output", true: "same input and output"}[samePath], func(t *testing.T) {
			dir := t.TempDir()
			input, output := filepath.Join(dir, "in.png"), filepath.Join(dir, "out.png")
			if samePath {
				output = input
			}
			for _, path := range []string{input, output} {
				if err := os.WriteFile(path, []byte("must survive"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			oldArgs, oldFlags := os.Args, flag.CommandLine
			defer func() { os.Args, flag.CommandLine = oldArgs, oldFlags }()
			os.Args = []string{"imgcompress", "-in", input, "-out", output}
			flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
			if err := run(); err == nil {
				t.Fatal("expected existing output error")
			}
			for _, path := range []string{input, output} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "must survive" {
					t.Fatalf("file changed: %q, %v", data, err)
				}
			}
		})
	}
}
