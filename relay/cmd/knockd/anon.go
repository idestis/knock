package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// allowAnonymous decides whether people can use the relay without signing
// in. KNOCK_ALLOW_ANONYMOUS wins when set. Unset, it follows the providers:
// a relay with no sign-in (local dev) is anonymous, and one with Google or
// GitHub configured requires sign-in.
func allowAnonymous(setting string, providers int) (bool, error) {
	setting = strings.TrimSpace(setting)
	if setting == "" {
		return providers == 0, nil
	}
	allow, err := strconv.ParseBool(setting)
	if err != nil {
		return false, fmt.Errorf("KNOCK_ALLOW_ANONYMOUS=%q: want true or false", setting)
	}
	if !allow && providers == 0 {
		return false, errors.New("KNOCK_ALLOW_ANONYMOUS=false needs a sign-in provider (KNOCK_GOOGLE_CLIENT_ID or KNOCK_GITHUB_CLIENT_ID), or nobody can connect")
	}
	return allow, nil
}
