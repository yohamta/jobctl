package util

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dagu-dev/dagu/internal/constants"
	"github.com/mattn/go-shellwords"
)

var (
	ErrUnexpectedEOF         = errors.New("unexpected end of input after escape character")
	ErrUnknownEscapeSequence = errors.New("unknown escape sequence")
)

// MustGetUserHomeDir returns current working directory.
// Panics is os.UserHomeDir() returns error
func MustGetUserHomeDir() string {
	hd, _ := os.UserHomeDir()
	return hd
}

// MustGetwd returns current working directory.
// Panics is os.Getwd() returns error
func MustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}

// FormatTime returns formatted time.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return constants.TimeEmpty
	}

	return t.Format(constants.TimeFormat)
}

// ParseTime parses time string.
func ParseTime(val string) (time.Time, error) {
	if val == constants.TimeEmpty {
		return time.Time{}, nil
	}
	return time.ParseInLocation(constants.TimeFormat, val, time.Local)
}

// FormatDuration returns formatted duration.
func FormatDuration(t time.Duration, defaultVal string) string {
	if t == 0 {
		return defaultVal
	}

	return t.String()
}

// SplitCommand splits command string to program and arguments.
// TODO: This function needs to be refactored to handle more complex cases.
func SplitCommand(cmd string, parse bool) (program string, args []string) {
	splits := strings.SplitN(cmd, " ", 2)
	if len(splits) == 1 {
		return splits[0], []string{}
	}
	program = splits[0]
	parser := shellwords.NewParser()
	parser.ParseBacktick = parse
	parser.ParseEnv = false

	a := EscapeSpecialChars(splits[1])
	args, err := parser.Parse(a)
	if err != nil {
		log.Printf("failed to parse arguments: %s", err)
		// if parse shell world error use all string as argument
		return program, []string{splits[1]}
	}

	var ret []string
	for _, v := range args {
		val := UnescapeSpecialChars(v)
		if parse {
			val = os.ExpandEnv(val)
		}
		ret = append(ret, val)
	}
	return program, ret
}

// AssignValues Assign values to command parameters
func AssignValues(command string, params map[string]string) string {
	updatedCommand := command

	for k, v := range params {
		updatedCommand = strings.ReplaceAll(updatedCommand, fmt.Sprintf("$%v", k), v)
	}

	return updatedCommand
}

// RemoveParams Returns a command with parameters stripped from it.
func RemoveParams(command string) string {
	paramRegex := regexp.MustCompile(`\$\w+`)

	return paramRegex.ReplaceAllString(command, "")
}

// ExtractParamNames extracts a slice of parameter names by removing the '$' from the command string.
func ExtractParamNames(command string) []string {
	words := strings.Fields(command)

	var params []string
	for _, word := range words {
		if strings.HasPrefix(word, "$") {
			paramName := strings.TrimPrefix(word, "$")
			params = append(params, paramName)
		}
	}

	return params
}

func UnescapeSpecialChars(str string) string {
	repl := strings.NewReplacer(
		`\\t`, `\t`,
		`\\r`, `\r`,
		`\\n`, `\n`,
	)
	return repl.Replace(str)
}

func EscapeSpecialChars(str string) string {
	repl := strings.NewReplacer(
		`\t`, `\\t`,
		`\r`, `\\r`,
		`\n`, `\\n`,
	)
	return repl.Replace(str)
}

// FileExists returns true if file exists.
func FileExists(file string) bool {
	_, err := os.Stat(file)
	return !os.IsNotExist(err)
}

// OpenOrCreateFile opens file or creates it if it doesn't exist.
func OpenOrCreateFile(file string) (*os.File, error) {
	if FileExists(file) {
		return OpenFile(file)
	}
	return CreateFile(file)
}

// OpenFile opens file.
func OpenFile(file string) (*os.File, error) {
	outfile, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0755)
	if err != nil {
		return nil, err
	}
	return outfile, nil
}

// CreateFile creates file.
func CreateFile(file string) (*os.File, error) {
	outfile, err := os.Create(file)
	if err != nil {
		return nil, err
	}
	return outfile, nil
}

// https://github.com/sindresorhus/filename-reserved-regex/blob/master/index.js
var (
	filenameReservedRegex             = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1F]`)
	filenameReservedWindowsNamesRegex = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])$`)
)

// ValidFilename returns true if filename is valid.
func ValidFilename(str, replacement string) string {
	s := filenameReservedRegex.ReplaceAllString(str, replacement)
	s = filenameReservedWindowsNamesRegex.ReplaceAllString(s, replacement)
	return strings.ReplaceAll(s, " ", replacement)
}

// MustTempDir returns temporary directory.
func MustTempDir(pattern string) string {
	t, err := os.MkdirTemp("", pattern)
	if err != nil {
		panic(err)
	}
	return t
}

// LogErr logs error if it's not nil.
func LogErr(action string, err error) {
	if err != nil {
		log.Printf("%s failed. %s", action, err)
	}
}

// TruncString TurnString returns truncated string.
func TruncString(val string, max int) string {
	if len(val) > max {
		return val[:max]
	}
	return val
}

// StringWithFallback StringsWithFallback returns the first non-empty string
// in the parameter list.
func StringWithFallback(val, fallback string) string {
	if val == "" {
		return fallback
	}
	return val
}

// MatchExtension returns true if extension matches.
func MatchExtension(file string, exts []string) bool {
	ext := filepath.Ext(file)
	for _, e := range exts {
		if e == ext {
			return true
		}
	}
	return false
}

var (
	fixedTime time.Time
	lock      sync.RWMutex
)

func SetFixedTime(t time.Time) {
	lock.Lock()
	defer lock.Unlock()
	fixedTime = t
}

func Now() time.Time {
	lock.RLock()
	defer lock.RUnlock()
	if fixedTime.IsZero() {
		return time.Now()
	}
	return fixedTime
}

func EscapeArg(input string, doubleQuotes bool) string {
	escaped := strings.Builder{}

	for _, char := range input {
		if char == '\r' {
			escaped.WriteString("\\r")
		} else if char == '\n' {
			escaped.WriteString("\\n")
		} else if char == '"' && doubleQuotes {
			escaped.WriteString("\\\"")
		} else {
			escaped.WriteRune(char)
		}
	}

	return escaped.String()
}

func UnescapeArg(input string) (string, error) {
	escaped := strings.Builder{}
	length := len(input)
	i := 0

	for i < length {
		char := input[i]

		if char == '\\' {
			i++
			if i >= length {
				return "", ErrUnexpectedEOF
			}

			switch input[i] {
			case 'n':
				escaped.WriteRune('\n')
			case 'r':
				escaped.WriteRune('\r')
			case '"':
				escaped.WriteRune('"')
			default:
				return "", fmt.Errorf("%w: '\\%c'", ErrUnknownEscapeSequence, input[i])
			}
		} else {
			escaped.WriteByte(char)
		}
		i++
	}

	return escaped.String(), nil
}
