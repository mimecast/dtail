package regex

import (
	"regexp"
)

type Flag int

const (
    // Default is the default regex mode (positive matching)
    Default = iota
    // Negative negates the regex 
    Negative = iota
    // Noop means no regex matching enabled, all defaults to true
    Noop = iota
)

type Regex struct {
    str string
    re *regexp.Regexp
    flag Flag
}

func (r Regex) Noop() Regex {
    return Regex{
        flag: Noop,
    }
}

func New(str string, flag Flag) (Regex, error) {
    r := Regex{
        str:  str,
        flag: flag,
    }

	re, err := regexp.Compile(str)

	if err != nil {
	    return r, err
	}

	r.re = re
    return r, nil
}

func (r Regex) MatchString(str string) bool {
    switch r.flag {
    case Default:
        return r.re.MatchString(str)
    case Negative:
        return !r.re.MatchString(str)
    case Noop:
        return true
    default:
        return false
    }
}

/*
func (r Regex) Serialize() string {
    // TODO: Serialize to hex encoded str
    return fmt.Sprintf("%b,%s",r.negate,r,str)
}

func (r Regex) Deserialize(input string) (Regex, error) {

}

*/
