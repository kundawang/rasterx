// Copyright 2026 The rasterx Authors. All rights reserved.
//
// This file proves the new deformation tests are sensitive: it makes a
// throwaway copy of the module, injects one defect at a time, runs a focused
// subset of the suite, and requires the run to fail in the expected test.
//
// The control run in the unmodified copy must pass, so a failure cannot be
// blamed on the harness itself. Nothing in the working tree is modified.
package rasterx_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type injectedDefect struct {
	name string
	file string
	// old must occur exactly once in file; new replaces it.
	old string
	new string
	// wantTests are test names that must appear in the failing output.
	wantTests []string
	// run is the -run regexp for the focused subset.
	run string
}

func injectedDefects() []injectedDefect {
	return []injectedDefect{
		{
			name: "matrix multiplication composes in the wrong order",
			file: "matrix.go",
			old: "		E: a.A*b.E + a.C*b.F + a.E,\n" +
				"		F: a.B*b.E + a.D*b.F + a.F}",
			new: "		E: b.A*a.E + b.C*a.F + b.E,\n" +
				"		F: b.B*a.E + b.D*a.F + b.F}",
			wantTests: []string{"TestMatrixCompositionOrder", "TestMatrixAdderChainsLikeComposedMatrix"},
			run:       "^(TestMatrixCompositionOrder|TestMatrixAdderChainsLikeComposedMatrix)$",
		},
		{
			name: "square cap is drawn at half the stroke half-width",
			file: "stroke.go",
			old: "		tpt := a.Add(turnStarboard90(eNorm))\n" +
				"		p.Start(a.Add(eNorm))\n" +
				"		p.Line(tpt.Add(eNorm))\n" +
				"		p.Line(tpt.Sub(eNorm))\n" +
				"		p.Line(a.Sub(eNorm))",
			new: "		tpt := a.Add(turnStarboard90(eNorm).Mul(1 << 5))\n" +
				"		p.Start(a.Add(eNorm))\n" +
				"		p.Line(tpt.Add(eNorm))\n" +
				"		p.Line(tpt.Sub(eNorm))\n" +
				"		p.Line(a.Sub(eNorm))",
			wantTests: []string{"TestStrokeCapAreas"},
			run:       "^(TestStrokeCapAreas)$",
		},
		{
			name:      "dash offset is scaled by 32 instead of 64",
			file:      "dash.go",
			old:       "		r.DashOffset = fixed.Int26_6(dashOffset * 64)",
			new:       "		r.DashOffset = fixed.Int26_6(dashOffset * 32)",
			wantTests: []string{"TestDashPeriodAndOffset", "TestDashRunsMatchWalker"},
			run:       "^(TestDashPeriodAndOffset|TestDashRunsMatchWalker)$",
		},
	}
}

const injectedSourceFiles = "LICENSE go.mod go.sum " +
	"bezier_test.go dash.go fill.go geomx.go gradient.go matrix.go rasterx_test.go scan.go shapes.go stroke.go " +
	"deformation_ref_test.go deformation_fixtures_test.go deformation_props_test.go deformation_dash_test.go mutation_test.go"

func copyDeformationModule(t *testing.T, dst string) {
	t.Helper()
	src, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Fields(injectedSourceFiles) {
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatalf("copy %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dst, "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(src, "testdata", "deformation_fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "testdata", "deformation_fixtures.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runFocusedTests(t *testing.T, dir, run string) (ok bool, output string) {
	t.Helper()
	cmd := exec.Command("go", "test", "-count=1", "-run", run, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOFLAGS=-mod=mod",
		"GOPROXY=off",
		"GOSUMDB=off",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return err == nil, buf.String()
}

// TestInjectedDefectsAreDetected injects three independent, localizable
// defects. Each must make the focused deformation tests fail; the unmodified
// control copy must pass.
func TestInjectedDefectsAreDetected(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	t.Run("control passes", func(t *testing.T) {
		dir := t.TempDir()
		copyDeformationModule(t, dir)
		ok, out := runFocusedTests(t, dir, "^(TestMatrixCompositionOrder|TestStrokeCapAreas|TestDashPeriodAndOffset|TestDashRunsMatchWalker)$")
		if !ok {
			t.Fatalf("unmodified copy failed the focused tests:\n%s", out)
		}
	})

	for _, defect := range injectedDefects() {
		defect := defect
		t.Run("defect: "+defect.name, func(t *testing.T) {
			dir := t.TempDir()
			copyDeformationModule(t, dir)

			path := filepath.Join(dir, defect.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(data), defect.old); n != 1 {
				t.Fatalf("defect patch for %s matches %d times in %s; the source moved", defect.name, n, defect.file)
			}
			patched := strings.Replace(string(data), defect.old, defect.new, 1)
			if patched == string(data) {
				t.Fatalf("defect patch for %s did not change %s", defect.name, defect.file)
			}
			if err := os.WriteFile(path, []byte(patched), 0o644); err != nil {
				t.Fatal(err)
			}

			ok, out := runFocusedTests(t, dir, defect.run)
			if ok {
				t.Fatalf("injected defect (%s) was not detected by %s", defect.name, defect.run)
			}
			matched := false
			for _, want := range defect.wantTests {
				if strings.Contains(out, "--- FAIL: "+want) {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("injected defect (%s) failed, but not in an expected test (%v):\n%s",
					defect.name, defect.wantTests, out)
			}
			t.Logf("defect %q caused failure in %v", defect.name, defect.wantTests)
		})
	}
}
