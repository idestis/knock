package state

import (
	"errors"
	"regexp"
	"strings"

	"github.com/idestis/knock/protocol"
)

const codeLen = 6

var (
	codeRe   = regexp.MustCompile(`^[0-9]{4}[A-HJ-NP-Z]{2}$`)
	doorIDRe = regexp.MustCompile(`^[a-z0-9]{4,32}$`)
)

// Errors from ParseShare, written for the person typing.
var (
	ErrNoDoor   = errors.New("Tell me who to knock on, with their code, like: knock BVJY 4821 KQ.")
	ErrBadDoor  = errors.New("That code doesn't look right. It looks like BVJY 4821 KQ.")
	ErrBadCode  = errors.New("That code doesn't look right. It's four digits and two letters, like 4821 KQ.")
	stripSpaces = strings.NewReplacer(" ", "", "-", "", "\t", "", "·", "")
)

// ValidCode reports whether code (already normalised) has the code shape.
func ValidCode(code string) bool { return codeRe.MatchString(code) }

// ParseShare turns what the guest typed into a door id and an optional code.
//
// Accepted forms (args are the command-line words after "knock" or "knock join"):
//
//	BVJY 4821 KQ           as shown in the app
//	bvjy 4821KQ            two words
//	"BVJY 4821 KQ"         one quoted word
//	bvjy-4821kq            dash
//	bvjy4821KQ             run together
//	k7f3x9q2 4821KQ        older 8-character doors still work
//	bvjy                   door only (code comes from Recent)
//
// Following docs/LOCAL_STATE.md, a single word is stripped of spaces and
// dashes; the last six characters are the code and the rest is the door id.
// A leading "knock" (pasted share string) is ignored.
func ParseShare(args []string) (doorID, code string, err error) {
	words := make([]string, 0, len(args))
	for _, a := range args {
		words = append(words, strings.Fields(a)...)
	}
	if len(words) > 0 && strings.EqualFold(words[0], "knock") {
		words = words[1:]
	}
	if len(words) == 0 {
		return "", "", ErrNoDoor
	}
	if len(words) == 1 {
		s := stripSpaces.Replace(words[0])
		if len(s) >= DoorIDLen+codeLen && ValidCode(strings.ToUpper(s[len(s)-codeLen:])) {
			doorID, code = s[:len(s)-codeLen], s[len(s)-codeLen:]
		} else {
			doorID = s
		}
	} else {
		doorID = stripSpaces.Replace(words[0])
		code = strings.Join(words[1:], "")
	}
	doorID = strings.ToLower(doorID)
	if !doorIDRe.MatchString(doorID) {
		return "", "", ErrBadDoor
	}
	if code != "" {
		code = protocol.NormaliseCode(code)
		if !ValidCode(code) {
			return "", "", ErrBadCode
		}
	}
	return doorID, code, nil
}
