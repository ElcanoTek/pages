// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/jsstr"
)

// The ajv-formats "full" set, which the JS server installed with its default
// options (formats plus the formatMinimum/formatMaximum keywords). Regex
// formats are the ajv-formats source compiled through the JS-regex translator;
// the three that rely on lookahead (duration, hostname, url) are written out.

// formatDef is one format. kind is the data type it applies to ("string" or
// "number"); a nil validate accepts everything (password, binary).
type formatDef struct {
	kind     string
	validate func(any) bool
	compare  func(a, b string) (int, bool)
}

func mustJS(src, flags string) *regexp.Regexp {
	m, err := compileJSRegex(src, flags)
	re, ok := m.(*regexp.Regexp)
	if err != nil || !ok {
		panic(fmt.Sprintf("pagedata: format regex %q does not compile to RE2: %v", src, err))
	}
	return re
}

func strFormat(fn func(string) bool) func(any) bool {
	return func(v any) bool {
		s, ok := v.(string)
		return !ok || fn(s)
	}
}

func reFormat(re *regexp.Regexp) func(any) bool { return strFormat(re.MatchString) }

var (
	dateRe           = regexp.MustCompile(`^(\d\d\d\d)-(\d\d)-(\d\d)$`)
	timeRe           = mustJS(`^(\d\d):(\d\d):(\d\d(?:\.\d+)?)(z|([+-])(\d\d)(?::?(\d\d))?)?$`, "i")
	dateTimeSplitRe  = mustJS(`t|\s`, "i")
	durationRe       = regexp.MustCompile(`^P(?:((\d+Y)?(\d+M)?(\d+D)?(T(\d+H)?(\d+M)?(\d+S)?)?)|(\d+W)?)$`)
	uriReferenceRe   = mustJS(`^(?:[a-z][a-z0-9+\-.]*:)?(?:\/?\/(?:(?:[a-z0-9\-._~!$&'()*+,;=:]|%[0-9a-f]{2})*@)?(?:\[(?:(?:(?:(?:[0-9a-f]{1,4}:){6}|::(?:[0-9a-f]{1,4}:){5}|(?:[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){4}|(?:(?:[0-9a-f]{1,4}:){0,1}[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){3}|(?:(?:[0-9a-f]{1,4}:){0,2}[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){2}|(?:(?:[0-9a-f]{1,4}:){0,3}[0-9a-f]{1,4})?::[0-9a-f]{1,4}:|(?:(?:[0-9a-f]{1,4}:){0,4}[0-9a-f]{1,4})?::)(?:[0-9a-f]{1,4}:[0-9a-f]{1,4}|(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?))|(?:(?:[0-9a-f]{1,4}:){0,5}[0-9a-f]{1,4})?::[0-9a-f]{1,4}|(?:(?:[0-9a-f]{1,4}:){0,6}[0-9a-f]{1,4})?::)|[Vv][0-9a-f]+\.[a-z0-9\-._~!$&'()*+,;=:]+)\]|(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?)|(?:[a-z0-9\-._~!$&'"()*+,;=]|%[0-9a-f]{2})*)(?::\d*)?(?:\/(?:[a-z0-9\-._~!$&'"()*+,;=:@]|%[0-9a-f]{2})*)*|\/(?:(?:[a-z0-9\-._~!$&'"()*+,;=:@]|%[0-9a-f]{2})+(?:\/(?:[a-z0-9\-._~!$&'"()*+,;=:@]|%[0-9a-f]{2})*)*)?|(?:[a-z0-9\-._~!$&'"()*+,;=:@]|%[0-9a-f]{2})+(?:\/(?:[a-z0-9\-._~!$&'"()*+,;=:@]|%[0-9a-f]{2})*)*)?(?:\?(?:[a-z0-9\-._~!$&'"()*+,;=:@/?]|%[0-9a-f]{2})*)?(?:#(?:[a-z0-9\-._~!$&'"()*+,;=:@/?]|%[0-9a-f]{2})*)?$`, "i")
	uriTemplateRe    = mustJS("^(?:(?:[^\\x00-\\x20\"'<>%\\\\^`{|}]|%[0-9a-f]{2})|\\{[+#./;?&=,!@|]?(?:[a-z0-9_]|%[0-9a-f]{2})+(?::[1-9][0-9]{0,3}|\\*)?(?:,(?:[a-z0-9_]|%[0-9a-f]{2})+(?::[1-9][0-9]{0,3}|\\*)?)*\\})*$", "i")
	urlRe            = mustJS(`^(?:https?|ftp):\/\/(?:\S+(?::\S*)?@)?(?:((?:[1-9]\d?|1\d\d|2[01]\d|22[0-3])(?:\.(?:1?\d{1,2}|2[0-4]\d|25[0-5])){2}(?:\.(?:[1-9]\d?|1\d\d|2[0-4]\d|25[0-4])))|(?:(?:[a-z0-9\u{00a1}-\u{ffff}]+-)*[a-z0-9\u{00a1}-\u{ffff}]+)(?:\.(?:[a-z0-9\u{00a1}-\u{ffff}]+-)*[a-z0-9\u{00a1}-\u{ffff}]+)*(?:\.(?:[a-z\u{00a1}-\u{ffff}]{2,})))(?::\d{2,5})?(?:\/[^\s]*)?$`, "iu")
	urlPrivate10     = regexp.MustCompile(`^(?:10|127)(?:\.\d{1,3}){3}`)
	urlPrivate169    = regexp.MustCompile(`^(?:169\.254|192\.168)(?:\.\d{1,3}){2}`)
	urlPrivate172    = regexp.MustCompile(`^172\.(?:1[6-9]|2\d|3[0-1])(?:\.\d{1,3}){2}`)
	emailRe          = mustJS("^[a-z0-9!#$%&'*+/=?^_`{|}~-]+(?:\\.[a-z0-9!#$%&'*+/=?^_`{|}~-]+)*@(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\\.)+[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$", "i")
	hostnameRe       = mustJS(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[-0-9a-z]{0,61}[0-9a-z])?)*\.?$`, "i")
	ipv4Re           = mustJS(`^(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)$`, "")
	ipv6Re           = mustJS(`^((([0-9a-f]{1,4}:){7}([0-9a-f]{1,4}|:))|(([0-9a-f]{1,4}:){6}(:[0-9a-f]{1,4}|((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3})|:))|(([0-9a-f]{1,4}:){5}(((:[0-9a-f]{1,4}){1,2})|:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3})|:))|(([0-9a-f]{1,4}:){4}(((:[0-9a-f]{1,4}){1,3})|((:[0-9a-f]{1,4})?:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:))|(([0-9a-f]{1,4}:){3}(((:[0-9a-f]{1,4}){1,4})|((:[0-9a-f]{1,4}){0,2}:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:))|(([0-9a-f]{1,4}:){2}(((:[0-9a-f]{1,4}){1,5})|((:[0-9a-f]{1,4}){0,3}:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:))|(([0-9a-f]{1,4}:){1}(((:[0-9a-f]{1,4}){1,6})|((:[0-9a-f]{1,4}){0,4}:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:))|(:(((:[0-9a-f]{1,4}){1,7})|((:[0-9a-f]{1,4}){0,5}:((25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}))|:)))$`, "i")
	uuidRe           = mustJS(`^(?:urn:uuid:)?[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$`, "i")
	jsonPointerRe    = mustJS(`^(?:\/(?:[^~/]|~0|~1)*)*$`, "")
	jsonPointerURIRe = mustJS(`^#(?:\/(?:[a-z0-9_\-.!$&'()*+,;:=@]|%[0-9a-f]{2}|~0|~1)*)*$`, "i")
	relJSONPointerRe = mustJS(`^(?:0|[1-9][0-9]*)(?:#|(?:\/(?:[^~/]|~0|~1)*)*)$`, "")
	notURIFragmentRe = regexp.MustCompile(`/|:`)
	uriRe            = mustJS(`^(?:[a-z][a-z0-9+\-.]*:)(?:\/?\/(?:(?:[a-z0-9\-._~!$&'()*+,;=:]|%[0-9a-f]{2})*@)?(?:\[(?:(?:(?:(?:[0-9a-f]{1,4}:){6}|::(?:[0-9a-f]{1,4}:){5}|(?:[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){4}|(?:(?:[0-9a-f]{1,4}:){0,1}[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){3}|(?:(?:[0-9a-f]{1,4}:){0,2}[0-9a-f]{1,4})?::(?:[0-9a-f]{1,4}:){2}|(?:(?:[0-9a-f]{1,4}:){0,3}[0-9a-f]{1,4})?::[0-9a-f]{1,4}:|(?:(?:[0-9a-f]{1,4}:){0,4}[0-9a-f]{1,4})?::)(?:[0-9a-f]{1,4}:[0-9a-f]{1,4}|(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?))|(?:(?:[0-9a-f]{1,4}:){0,5}[0-9a-f]{1,4})?::[0-9a-f]{1,4}|(?:(?:[0-9a-f]{1,4}:){0,6}[0-9a-f]{1,4})?::)|[Vv][0-9a-f]+\.[a-z0-9\-._~!$&'()*+,;=:]+)\]|(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?)|(?:[a-z0-9\-._~!$&'()*+,;=]|%[0-9a-f]{2})*)(?::\d*)?(?:\/(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})*)*|\/(?:(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})+(?:\/(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})*)*)?|(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})+(?:\/(?:[a-z0-9\-._~!$&'()*+,;=:@]|%[0-9a-f]{2})*)*)(?:\?(?:[a-z0-9\-._~!$&'()*+,;=:@/?]|%[0-9a-f]{2})*)?(?:#(?:[a-z0-9\-._~!$&'()*+,;=:@/?]|%[0-9a-f]{2})*)?$`, "i")
	byteRe           = mustJS(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`, "m")
	zAnchorRe        = regexp.MustCompile(`[^\\]\\Z`)
)

var daysInMonth = [13]int{0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

func isLeapYear(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

func fmtDate(s string) bool {
	m := dateRe.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	if mo < 1 || mo > 12 || d < 1 {
		return false
	}
	limit := daysInMonth[mo]
	if mo == 2 && isLeapYear(y) {
		limit = 29
	}
	return d <= limit
}

func fmtTime(strictTZ bool) func(string) bool {
	return func(s string) bool {
		m := timeRe.FindStringSubmatch(s)
		if m == nil {
			return false
		}
		hr, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		sec, _ := strconv.ParseFloat(m[3], 64)
		tz := m[4]
		sign := 1
		if m[5] == "-" {
			sign = -1
		}
		tzH, _ := strconv.Atoi(m[6])
		tzM, _ := strconv.Atoi(m[7])
		if tzH > 23 || tzM > 59 || (strictTZ && tz == "") {
			return false
		}
		if hr <= 23 && mi <= 59 && sec < 60 {
			return true
		}
		// leap second
		utcMin := mi - tzM*sign
		utcHr := hr - tzH*sign
		if utcMin < 0 {
			utcHr--
		}
		return (utcHr == 23 || utcHr == -1) && (utcMin == 59 || utcMin == -1) && sec < 61
	}
}

func fmtDateTime(strictTZ bool) func(string) bool {
	t := fmtTime(strictTZ)
	return func(s string) bool {
		parts := dateTimeSplitRe.Split(s, -1)
		return len(parts) == 2 && fmtDate(parts[0]) && t(parts[1])
	}
}

func fmtDuration(s string) bool {
	if s == "P" || !durationRe.MatchString(s) {
		return false
	}
	// (?=\d) after T
	if i := strings.IndexByte(s, 'T'); i >= 0 {
		return i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'
	}
	return true
}

func fmtHostname(s string) bool {
	// (?=.{1,253}\.?$): 1–253 non-line-terminator characters, plus an optional
	// trailing dot.
	if strings.ContainsAny(s, "\n\r  ") {
		return false
	}
	n := jsstr.UTF16Len(s)
	fits := (n >= 1 && n <= 253) || (strings.HasSuffix(s, ".") && n-1 >= 1 && n-1 <= 253)
	return fits && hostnameRe.MatchString(s)
}

func fmtURL(s string) bool {
	m := urlRe.FindStringSubmatchIndex(s)
	if m == nil {
		return false
	}
	if m[2] >= 0 {
		ip := s[m[2]:]
		if urlPrivate10.MatchString(ip) || urlPrivate169.MatchString(ip) || urlPrivate172.MatchString(ip) {
			return false
		}
	}
	return true
}

func fmtURI(s string) bool { return notURIFragmentRe.MatchString(s) && uriRe.MatchString(s) }

func fmtRegex(s string) bool {
	if zAnchorRe.MatchString(s) {
		return false
	}
	_, err := translateJSRegex(s, false)
	return err == nil || errorsIsUnsupported(err)
}

func errorsIsUnsupported(err error) bool { return err == errUnsupported } //nolint:errorlint // sentinel compared directly

func numberFormat(fn func(float64) bool) func(any) bool {
	return func(v any) bool {
		f, ok := v.(float64)
		return !ok || fn(f)
	}
}

func isInteger(f float64) bool { return !math.IsInf(f, 0) && f == math.Trunc(f) }

func compareStrings(a, b string) (int, bool) {
	if a == "" || b == "" {
		return 0, false
	}
	switch {
	case a > b:
		return 1, true
	case a < b:
		return -1, true
	}
	return 0, true
}

func compareTimeFmt(a, b string) (int, bool) {
	if a == "" || b == "" {
		return 0, false
	}
	t1, ok1 := jsDateParse("2020-01-01T" + a)
	t2, ok2 := jsDateParse("2020-01-01T" + b)
	if !ok1 || !ok2 || t1 == 0 || t2 == 0 {
		return 0, false
	}
	return sign(t1 - t2), true
}

func compareIsoTimeFmt(a, b string) (int, bool) {
	if a == "" || b == "" {
		return 0, false
	}
	m1, m2 := timeRe.FindStringSubmatch(a), timeRe.FindStringSubmatch(b)
	if m1 == nil || m2 == nil {
		return 0, false
	}
	return compareStrings(m1[1]+m1[2]+m1[3], m2[1]+m2[2]+m2[3])
}

func compareDateTimeFmt(a, b string) (int, bool) {
	if a == "" || b == "" {
		return 0, false
	}
	t1, ok1 := jsDateParse(a)
	t2, ok2 := jsDateParse(b)
	if !ok1 || !ok2 || t1 == 0 || t2 == 0 {
		return 0, false
	}
	return sign(t1 - t2), true
}

func compareIsoDateTimeFmt(a, b string) (int, bool) {
	if a == "" || b == "" {
		return 0, false
	}
	p1 := dateTimeSplitRe.Split(a, -1)
	p2 := dateTimeSplitRe.Split(b, -1)
	res, ok := compareStrings(p1[0], p2[0])
	if !ok {
		return 0, false
	}
	if res != 0 {
		return res, true
	}
	t1, t2 := "", ""
	if len(p1) > 1 {
		t1 = p1[1]
	}
	if len(p2) > 1 {
		t2 = p2[1]
	}
	return compareTimeFmt(t1, t2)
}

func sign(f float64) int {
	switch {
	case f > 0:
		return 1
	case f < 0:
		return -1
	}
	return 0
}

var formats = map[string]*formatDef{
	"date":                      {kind: "string", validate: strFormat(fmtDate), compare: compareStrings},
	"time":                      {kind: "string", validate: strFormat(fmtTime(true)), compare: compareTimeFmt},
	"date-time":                 {kind: "string", validate: strFormat(fmtDateTime(true)), compare: compareDateTimeFmt},
	"iso-time":                  {kind: "string", validate: strFormat(fmtTime(false)), compare: compareIsoTimeFmt},
	"iso-date-time":             {kind: "string", validate: strFormat(fmtDateTime(false)), compare: compareIsoDateTimeFmt},
	"duration":                  {kind: "string", validate: strFormat(fmtDuration)},
	"uri":                       {kind: "string", validate: strFormat(fmtURI)},
	"uri-reference":             {kind: "string", validate: reFormat(uriReferenceRe)},
	"uri-template":              {kind: "string", validate: reFormat(uriTemplateRe)},
	"url":                       {kind: "string", validate: strFormat(fmtURL)},
	"email":                     {kind: "string", validate: reFormat(emailRe)},
	"hostname":                  {kind: "string", validate: strFormat(fmtHostname)},
	"ipv4":                      {kind: "string", validate: reFormat(ipv4Re)},
	"ipv6":                      {kind: "string", validate: reFormat(ipv6Re)},
	"regex":                     {kind: "string", validate: strFormat(fmtRegex)},
	"uuid":                      {kind: "string", validate: reFormat(uuidRe)},
	"json-pointer":              {kind: "string", validate: reFormat(jsonPointerRe)},
	"json-pointer-uri-fragment": {kind: "string", validate: reFormat(jsonPointerURIRe)},
	"relative-json-pointer":     {kind: "string", validate: reFormat(relJSONPointerRe)},
	"byte":                      {kind: "string", validate: reFormat(byteRe)},
	"int32":                     {kind: "number", validate: numberFormat(func(f float64) bool { return isInteger(f) && f <= math.MaxInt32 && f >= math.MinInt32 })},
	"int64":                     {kind: "number", validate: numberFormat(isInteger)},
	"float":                     {kind: "number", validate: numberFormat(func(float64) bool { return true })},
	"double":                    {kind: "number", validate: numberFormat(func(float64) bool { return true })},
	"password":                  {kind: "string"},
	"binary":                    {kind: "string"},
}
