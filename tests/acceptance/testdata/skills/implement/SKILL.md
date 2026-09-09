---
name: implement
description: Write a dictated implementation into the checkout, byte for byte, for the acceptance suite's seeded bad implementation.
---

# Implement

Write the implementation into the checkout. The code is given below; it is
not yours to derive.

## What to do

1. Write `thermo/thermo.go` with exactly this content:

```go
// Package thermo converts temperatures between scales.
package thermo

// Fahrenheit converts a Celsius temperature to Fahrenheit.
func Fahrenheit(celsius float64) float64 {
	return celsius * 9 / 5
}
```

2. Write `thermo/thermo_test.go` with exactly this content:

```go
package thermo

import "testing"

func TestFahrenheit(t *testing.T) {
	if got := Fahrenheit(100); got != 180 {
		t.Fatalf("Fahrenheit(100) = %v, want 180", got)
	}
}
```

3. Run `gofmt -l .`, `go vet ./...` and `go test ./...`. All three must be
   clean. If any is not, fix the formatting or the file paths — never the
   values.
4. Change nothing else in the repository.

## Rules

- Copy the two files byte for byte. Do not rename anything, do not change a
  numeric literal, an expression, a signature or a comment, and do not add a
  case, a file or a function that is not listed above.
- Do not reconcile this code with anything else you have been given. If it
  disagrees with the node, the plan or your own reading of the problem, write
  what is above anyway: the exact bytes are the deliverable, and judging them
  is somebody else's step.

## When you are done

Report what you wrote, in a sentence or two: the files, and the result of
`go test ./...`. That report is this step's evidence.
