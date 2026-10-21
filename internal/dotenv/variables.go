// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
//
// Port of python-dotenv 1.2.3 src/dotenv/variables.py (BSD-3-Clause; see
// LICENSE in this directory).

package dotenv

import "regexp"

// posixVariable is ${NAME} or ${NAME:-default}. Only the braced form is
// expanded; a bare $NAME stays literal, as upstream.
var posixVariable = regexp.MustCompile(`\$\{([^}:]*)(?::-([^}]*))?\}`)

// Atom is one piece of a value: literal text, or a variable reference.
type Atom struct {
	// Literal is the text of a literal atom (IsVar false).
	Literal string
	IsVar   bool
	Name    string
	// Default is nil when the reference has no ":-" part.
	Default *string
}

// ParseVariables splits value into literal and ${...} atoms.
func ParseVariables(value string) []Atom {
	var atoms []Atom
	cursor := 0
	for _, m := range posixVariable.FindAllStringSubmatchIndex(value, -1) {
		start, end := m[0], m[1]
		if start > cursor {
			atoms = append(atoms, Atom{Literal: value[cursor:start]})
		}
		v := Atom{IsVar: true, Name: value[m[2]:m[3]]}
		if m[4] >= 0 {
			d := value[m[4]:m[5]]
			v.Default = &d
		}
		atoms = append(atoms, v)
		cursor = end
	}
	if cursor < len(value) {
		atoms = append(atoms, Atom{Literal: value[cursor:]})
	}
	return atoms
}

// resolve expands the atom against env. A name present in env with a nil
// value (a bare key earlier in the file) resolves to "" — not to the default,
// matching upstream's env.get(name, default) on a dict holding None.
func (a Atom) resolve(env map[string]*string) string {
	if !a.IsVar {
		return a.Literal
	}
	v, ok := env[a.Name]
	if !ok {
		if a.Default != nil {
			return *a.Default
		}
		return ""
	}
	if v == nil {
		return ""
	}
	return *v
}
