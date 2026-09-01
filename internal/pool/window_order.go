package pool

import (
	"strconv"
	"strings"
)

// windowRank orders quota windows for display: by the window's own length
// first, then base before variant, then alphabetically.
//
// Sorting on a parsed duration rather than the name is deliberate. Lexically
// "5h" sorts after "7d", which is already wrong for the two windows every
// Claude account has, and it would break again the moment a "24h" or "30d"
// window appears — exactly the kind of silent regression a name-sort invites.
//
// A name whose leading duration cannot be parsed (Kimi's "weekly", or
// anything a provider adds later) sorts last rather than being guessed at,
// alphabetically so the order is still stable and predictable.
func windowRank(name string) (minutes int, variant string, known bool) {
	base, variant, _ := strings.Cut(name, "-")
	if base == "" {
		return 0, variant, false
	}
	unit := base[len(base)-1]
	n, err := strconv.Atoi(base[:len(base)-1])
	if err != nil || n <= 0 {
		return 0, variant, false
	}
	switch unit {
	case 'm':
		return n, variant, true
	case 'h':
		return n * 60, variant, true
	case 'd':
		return n * 60 * 24, variant, true
	case 'w':
		return n * 60 * 24 * 7, variant, true
	}
	return 0, variant, false
}

// lessWindow reports whether window a sorts before window b.
func lessWindow(a, b string) bool {
	am, av, aok := windowRank(a)
	bm, bv, bok := windowRank(b)
	// Unparseable names go last, but keep a stable order among themselves.
	if aok != bok {
		return aok
	}
	if !aok {
		return a < b
	}
	if am != bm {
		return am < bm
	}
	// Same length: the base window before its variants ("7d" before
	// "7d-fable"), then alphabetically so two variants never swap places.
	if (av == "") != (bv == "") {
		return av == ""
	}
	return av < bv
}
