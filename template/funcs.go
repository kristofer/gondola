package template

// Functions available to gondola templates

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"reflect"
	"strings"
	"time"

	"gnd.la/app/serialize"
	"gnd.la/html"
	"gnd.la/util/types"
)

func eq(args ...interface{}) bool {
	if len(args) == 0 {
		return false
	}
	x := args[0]
	switch x := x.(type) {
	case string, int, int64, byte, float32, float64:
		for _, y := range args[1:] {
			if x == y {
				return true
			}
		}
		return false
	}

	for _, y := range args[1:] {
		if reflect.DeepEqual(x, y) {
			return true
		}
	}
	return false
}

func neq(args ...interface{}) bool {
	return !eq(args...)
}

func lt(arg1, arg2 interface{}) (bool, error) {
	v1 := reflect.ValueOf(arg1)
	v2 := reflect.ValueOf(arg2)
	t1 := v1.Type()
	t2 := v2.Type()
	switch {
	case types.IsInt(t1) && types.IsInt(t2):
		return v1.Int() < v2.Int(), nil
	case types.IsUint(t1) && types.IsUint(t2):
		return v1.Uint() < v2.Uint(), nil
	case types.IsFloat(t1) && types.IsFloat(t2):
		return v1.Float() < v2.Float(), nil
	}
	return false, fmt.Errorf("can't compare %T with %T", arg1, arg2)
}

func lte(arg1, arg2 interface{}) (bool, error) {
	lessThan, err := lt(arg1, arg2)
	if lessThan || err != nil {
		return lessThan, err
	}
	return eq(arg1, arg2), nil
}

func gt(arg1, arg2 interface{}) (bool, error) {
	v1 := reflect.ValueOf(arg1)
	v2 := reflect.ValueOf(arg2)
	t1 := v1.Type()
	t2 := v2.Type()
	switch {
	case types.IsInt(t1) && types.IsInt(t2):
		return v1.Int() > v2.Int(), nil
	case types.IsUint(t1) && types.IsUint(t2):
		return v1.Uint() > v2.Uint(), nil
	case types.IsFloat(t1) && types.IsFloat(t2):
		return v1.Float() > v2.Float(), nil
	}
	return false, fmt.Errorf("can't compare %T with %T", arg1, arg2)
}

func gte(arg1, arg2 interface{}) (bool, error) {
	greaterThan, err := gt(arg1, arg2)
	if greaterThan || err != nil {
		return greaterThan, err
	}
	return eq(arg1, arg2), nil
}

func jsons(arg interface{}) (string, error) {
	if jw, ok := arg.(serialize.JSONWriter); ok {
		var buf bytes.Buffer
		_, err := jw.WriteJSON(&buf)
		return buf.String(), err
	}
	b, err := json.Marshal(arg)
	return string(b), err
}

func _json(arg interface{}) (template.JS, error) {
	s, err := jsons(arg)
	return template.JS(s), err
}

func nz(x interface{}) bool {
	switch x := x.(type) {
	case int, uint, int64, uint64, byte, float32, float64:
		return x != 0
	case string:
		return len(x) > 0
	}
	return false
}

func _map(args ...interface{}) (map[string]interface{}, error) {
	var key string
	m := make(map[string]interface{})
	for ii, v := range args {
		if ii%2 == 0 {
			if s, ok := v.(string); ok {
				key = s
			} else if s, ok := v.(*string); ok {
				key = *s
			} else {
				return nil, fmt.Errorf("invalid argument to map at index %d, %s instead of string", ii, reflect.TypeOf(v))
			}
		} else {
			m[key] = v
		}
	}
	return m, nil
}

// this returns *[]interface{} so append works on
// slices declared in templates
func _slice(args ...interface{}) *[]interface{} {
	// We need to copy the slice, since state.call
	// reuses a []interface{} to make all the calls
	// to variadic functions.
	ret := make([]interface{}, len(args))
	copy(ret, args)
	return &ret
}

func _append(items interface{}, args ...interface{}) (string, error) {
	val := reflect.ValueOf(items)
	if !val.IsValid() || val.Kind() != reflect.Ptr || val.Elem().Kind() != reflect.Slice {
		return "", fmt.Errorf("first argument to append must be pointer to slice, it's %T", items)
	}
	sl := val.Elem()
	for _, v := range args {
		vval := reflect.ValueOf(v)
		if !vval.Type().AssignableTo(sl.Type().Elem()) {
			return "", fmt.Errorf("can't append %s to %s", vval.Type(), sl.Type())
		}
		sl = reflect.Append(sl, vval)
	}
	val.Elem().Set(sl)
	return "", nil
}

func _indirect(arg interface{}) interface{} {
	v := reflect.ValueOf(arg)
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			v = reflect.New(v.Type().Elem())
		}
		v = v.Elem()
	}
	return v.Interface()
}

type numericFloatFunc func(args ...interface{}) (float64, error)
type numericIntFunc func(args ...interface{}) (int, error)
type numericIfaceFunc func(args ...interface{}) (interface{}, error)

type operator func(float64, float64) float64

func numberToIface(f float64) interface{} {
	if _, frac := math.Modf(f); frac == 0 {
		return int(f)
	}
	return f
}

func numericFunctions(n float64, op operator) (numericFloatFunc, numericIntFunc, numericIfaceFunc) {
	floatFunc := func(args ...interface{}) (float64, error) {
		if len(args) == 0 {
			return n, nil
		}
		total, err := types.ToFloat(args[0])
		if err != nil {
			return 0, err
		}
		for _, v := range args[1:] {
			val, err := types.ToFloat(v)
			if err != nil {
				return 0, err
			}
			total = op(total, val)
		}
		return total, nil
	}
	intFunc := func(args ...interface{}) (int, error) {
		val, err := floatFunc(args...)
		return int(val), err
	}
	ifaceFunc := func(args ...interface{}) (interface{}, error) {
		val, err := floatFunc(args...)
		return numberToIface(val), err
	}
	return floatFunc, intFunc, ifaceFunc
}

var (
	mulf, muli, mul = numericFunctions(1.0, func(a, b float64) float64 { return a * b })
	addf, addi, add = numericFunctions(0.0, func(a, b float64) float64 { return a + b })
	subf, subi, sub = numericFunctions(0.0, func(a, b float64) float64 { return a - b })
)

func concat(args ...interface{}) string {
	s := make([]string, len(args))
	for ii, v := range args {
		s[ii] = types.ToString(v)
	}
	return strings.Join(s, "")
}

func and(args ...interface{}) interface{} {
	for _, v := range args {
		t, _ := types.IsTrue(v)
		if !t {
			return v
		}
	}
	return args[len(args)-1]
}

func or(args ...interface{}) interface{} {
	for _, v := range args {
		t, _ := types.IsTrue(v)
		if t {
			return v
		}
	}
	return args[len(args)-1]
}

func not(arg interface{}) bool {
	t, _ := types.IsTrue(arg)
	return !t
}

func divisible(n interface{}, d interface{}) (bool, error) {
	ni, err := types.ToInt(n)
	if err != nil {
		return false, fmt.Errorf("divisible() invalid number %v: %s", n, err)
	}
	di, err := types.ToInt(d)
	if err != nil {
		return false, fmt.Errorf("divisible() invalid divisor %v: %s", d, err)
	}
	return ni%di == 0, nil
}

func even(arg interface{}) (bool, error) {
	return divisible(arg, 2)
}

func odd(arg interface{}) (bool, error) {
	res, err := divisible(arg, 2)
	if err != nil {
		return false, err
	}
	return !res, nil
}

func now() time.Time {
	return time.Now()
}

func toHtml(s string) template.HTML {
	return template.HTML(strings.Replace(html.Escape(s), "\n", "<br>", -1))
}

func getVar(s *State, name string) interface{} {
	v, ok := s.Var(name)
	if !ok || !v.IsValid() {
		return nil
	}
	return v.Interface()
}

var templateFuncs = makeFuncMap(FuncMap{
	// Returns true iff the first argument is equal to any of the
	// following ones.
	"#eq": eq,
	// Returns true iff the first argument is different to all the following
	// ones.
	"#neq": neq,
	// Returns true iff arg1 < arg2. Produces an error if arguments are of
	// different types of if its type is not comparable.
	"#lt": lt,
	// Returns true iff arg1 <= arg2. Produces an error if arguments are of
	// different types of if its type is not comparable.
	"#lte": lte,
	// Returns true iff arg1 > arg2. Produces an error if arguments are of
	// different types of if its type is not comparable.
	"#gt": gt,
	// Returns true iff arg1 >= arg2. Produces an error if arguments are of
	// different types of if its type is not comparable.
	"#gte": gte,
	// Returns the JSON representation of the given argument as a string.
	// Produces an error in the argument can't be converted to JSON.
	"#jsons": jsons,
	// Same as jsons, but returns a template.JS, which can be embedded in script
	// sections of an HTML template without further escaping.
	"#json": _json,
	"#nz":   nz,
	"#join": strings.Join,
	"#map":  _map,
	// Returns a slice with the given arguments.
	"#slice":    _slice,
	"#append":   _append,
	"#indirect": _indirect,
	// Add all the arguments, returning a float64.
	"#addf": addf,
	// Add all the arguments, returning either an int (if the result does not
	// have a decimal part) or a float64.
	"#add": add,
	// Add all the arguments, returning a int. If the result has a decimal part,
	// it's truncated.
	"#addi": addi,
	// Substract all the arguments in the given order, from left to right, returning a float64.
	"#subf": subf,
	// Substract all the arguments in the given order, from left to right, returning either an int
	// (if the result does not have a decimal part) or a float64.
	"#sub": sub,
	// Substract all the arguments in the given order, from left to right, returning a int. If the
	// result has a decimal part, it's truncated.
	"#subi": subi,
	// Multiply all the arguments, returning a float64.
	"#mulf": mulf,
	// Multiply all the arguments, returning either an int (if the result does not
	// have a decimal part) or a float64.
	"#mul": mul,
	// Multiply all the arguments, returning a int. If the result has a decimal part,
	// it's truncated.
	"#muli": muli,
	// Returns true if the first argument is divisible by the second one.
	"#divisible": divisible,
	// An alias for divisible(arg, 2)
	"#even": even,
	// An alias for not divisible(arg, 2)
	"#odd": odd,
	// Return the result of concatenating all the arguments.
	"#concat": concat,
	// Return the last argument of the given ones if all of them are true. Otherwise,
	// return the first non-true one.
	"#and": and,
	// Return the first true argument of the given ones. If none of them is true,
	// return false.
	"#or": or,
	// Return the negation of the truth value of the given argument.
	"#not": not,
	// Return the current time.Time in the local timezone.
	"now":       now,
	"#int":      types.ToInt,
	"#float":    types.ToFloat,
	"#split":    strings.Split,
	"#split_n":  strings.SplitN,
	"#to_lower": strings.ToLower,
	"#to_title": strings.ToTitle,
	"#to_upper": strings.ToUpper,
	// Converts plain text to HTML by escaping it and replacing
	// newlines with <br> tags.
	"#to_html": toHtml,

	// !state manipulation functions

	// Return the value of the given variable, or an empty
	// value if no such variable exists.
	"@var": getVar,

	// !Go builtins
	"call":  call,
	"#html": template.HTMLEscaper,
	// Return the result of indexing into the first argument, which must be a map or slice,
	// using the second one (i.e. item[idx]).
	"#index": index,
	"#js":    template.JSEscaper,
	// Return the length of the argument, which must be map, slice or array.
	"#len":      length,
	"#print":    fmt.Sprint,
	"#printf":   fmt.Sprintf,
	"#println":  fmt.Sprintln,
	"#urlquery": template.URLQueryEscaper,

	// !Pseudo-functions which act as custom tags
	"extend": nop,
	// !Used to make the parser parse undefined
	// variables, since we allow variable
	// inheritance to subtemplates
	varNop: nop,
})
