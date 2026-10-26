package main

import (
	"errors"
	"fmt"
	gomath "math"
	"math/big"
	"strings"
	"time"

	"go.starlark.net/lib/math"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// ---------------------------------------------------------------------------
// Rechnen mit Code: Starlark ist ein Python-Dialekt, der komplett abgeschottet
// laeuft. Kein Dateizugriff, kein Netz, keine Imports, kein Zugriff auf den
// PC. Dazu ein Schritt- und Zeitlimit, damit Endlosschleifen nichts blockieren.
// ---------------------------------------------------------------------------

const (
	codeMaxSteps  = 50_000_000
	codeTimeout   = 10 * time.Second
	codeMaxOutput = 4000
	codeMaxSource = 4_000 // Rechenprogramme sind kurz; lange Texte gehoeren nicht in den Code
)

var codeFileOptions = &syntax.FileOptions{
	While:           true,
	Recursion:       true,
	Set:             true,
	TopLevelControl: true,
	GlobalReassign:  true,
}

func runStarlark(src string) string {
	src = strings.TrimSpace(src)
	src = strings.TrimPrefix(src, "```python")
	src = strings.TrimPrefix(src, "```starlark")
	src = strings.TrimPrefix(src, "```")
	src = strings.TrimSuffix(src, "```")
	if strings.TrimSpace(src) == "" {
		return "Error: no code."
	}
	if len(src) > codeMaxSource {
		return "Error: program too long. run_code is only for short calculations. Do not paste long texts into code and do not measure your own answer – just write the answer now."
	}

	var out strings.Builder
	truncated := false
	thread := &starlark.Thread{
		Name: "klaus",
		Print: func(_ *starlark.Thread, msg string) {
			if out.Len() > codeMaxOutput {
				truncated = true
				return
			}
			out.WriteString(msg)
			out.WriteByte('\n')
		},
		Load: func(*starlark.Thread, string) (starlark.StringDict, error) {
			return nil, errors.New("load() is not available")
		},
	}
	thread.SetMaxExecutionSteps(codeMaxSteps)
	timer := time.AfterFunc(codeTimeout, func() { thread.Cancel("time limit (10 s) reached") })
	defer timer.Stop()

	predeclared := starlark.StringDict{
		"math":  math.Module,
		"pow":   starlark.NewBuiltin("pow", builtinPow),
		"sum":   starlark.NewBuiltin("sum", builtinSum),
		"round": starlark.NewBuiltin("round", builtinRound),
	}
	_, err := starlark.ExecFileOptions(codeFileOptions, thread, "code.star", src, predeclared)

	res := out.String()
	if len(res) > codeMaxOutput {
		res = res[:codeMaxOutput]
	}
	if truncated {
		res += "\n[output truncated]"
	}
	if err != nil {
		msg := err.Error()
		var evalErr *starlark.EvalError
		if errors.As(err, &evalErr) {
			msg = evalErr.Backtrace()
		}
		if strings.Contains(msg, "'**'") {
			msg += "\n(Starlark has no ** operator: use pow(a, b) instead.)"
		}
		if res != "" {
			return fmt.Sprintf("Output so far:\n%s\nError: %s", res, msg)
		}
		return "Error: " + msg
	}
	if strings.TrimSpace(res) == "" {
		return "(no output – use print() to show results)"
	}
	return res
}

// pow(a, b[, mod]): exakt fuer ganze Zahlen (beliebig gross), sonst float.
func builtinPow(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var a, b starlark.Value
	var mod starlark.Value = starlark.None
	if err := starlark.UnpackPositionalArgs(fn.Name(), args, kwargs, 2, &a, &b, &mod); err != nil {
		return nil, err
	}
	ai, aok := a.(starlark.Int)
	bi, bok := b.(starlark.Int)
	if aok && bok && bi.Sign() >= 0 {
		if e, ok := bi.Int64(); ok && e > 100_000 && mod == starlark.None {
			return nil, errors.New("pow: exponent too large")
		}
		var m *big.Int
		if mi, ok := mod.(starlark.Int); ok {
			m = mi.BigInt()
		}
		return starlark.MakeBigInt(new(big.Int).Exp(ai.BigInt(), bi.BigInt(), m)), nil
	}
	x, ok1 := starlark.AsFloat(a)
	y, ok2 := starlark.AsFloat(b)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("pow: need numbers, got %s and %s", a.Type(), b.Type())
	}
	return starlark.Float(gomath.Pow(x, y)), nil
}

// sum(iterable, start=0)
func builtinSum(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var it starlark.Iterable
	var acc starlark.Value = starlark.MakeInt(0)
	if err := starlark.UnpackPositionalArgs(fn.Name(), args, kwargs, 1, &it, &acc); err != nil {
		return nil, err
	}
	iter := it.Iterate()
	defer iter.Done()
	var x starlark.Value
	for iter.Next(&x) {
		v, err := starlark.Binary(syntax.PLUS, acc, x)
		if err != nil {
			return nil, err
		}
		acc = v
	}
	return acc, nil
}

// round(x, digits=0) wie in Python (kaufmaennisch bei .5 weg von 0).
func builtinRound(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var x starlark.Value
	digits := starlark.Value(starlark.None)
	if err := starlark.UnpackPositionalArgs(fn.Name(), args, kwargs, 1, &x, &digits); err != nil {
		return nil, err
	}
	f, ok := starlark.AsFloat(x)
	if !ok {
		return nil, fmt.Errorf("round: need a number, got %s", x.Type())
	}
	if digits == starlark.None {
		return starlark.MakeInt64(int64(gomath.Round(f))), nil
	}
	d, err := starlark.AsInt32(digits)
	if err != nil {
		return nil, err
	}
	p := gomath.Pow(10, float64(d))
	return starlark.Float(gomath.Round(f*p) / p), nil
}
