package voice

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agent-platform/internal/contracts"
	"agent-platform/internal/toolinteraction"
)

var errUnclearAnswer = errors.New("答案不够明确，请再说一次")
var arabicNumber = regexp.MustCompile(`^[+-]?\d+(?:\.\d+)?$`)
var optionSeparators = regexp.MustCompile(`[,，、和与及]`)
var chineseDate = regexp.MustCompile(`^([0-9零〇一二三四五六七八九]{4})年([0-9一二三四五六七八九十]{1,3})月([0-9一二三四五六七八九十]{1,3})[日号](.*)$`)
var spokenClock = regexp.MustCompile(`^([0-9零〇一二两三四五六七八九十]{1,3})[点时](?:(半)|([0-9零〇一二两三四五六七八九十]{1,3})分?)?$`)
var colonClock = regexp.MustCompile(`^(\d{1,2}):(\d{2})(?::(\d{2}))?$`)

// convertQuestionAnswer uses deliberately finite rules. Runtime validates the
// result again against the current question under its existing awaiting lock.
func convertQuestionAnswer(question map[string]any, transcript, zone string, finalized time.Time) (map[string]any, error) {
	text := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(transcript), "。！？!?"))
	if text == "" {
		return nil, errUnclearAnswer
	}
	item := map[string]any{"id": contracts.AnyStringNode(question["id"])}
	kind := strings.ToLower(strings.TrimSpace(contracts.AnyStringNode(question["type"])))
	switch kind {
	case "text":
		item["answer"] = text
	case "number":
		value, err := parseSpokenNumber(text)
		if err != nil {
			return nil, err
		}
		item["answer"] = value
	case "date", "datetime":
		value, err := parseSpokenDate(text, kind, zone, finalized)
		if err != nil {
			return nil, err
		}
		item["answer"] = value
	case "select", "multi-select":
		values, err := parseSpokenOptions(question, text)
		if err != nil {
			return nil, err
		}
		if kind == "select" {
			if len(values) != 1 {
				return nil, errUnclearAnswer
			}
			item["answer"] = values[0]
		} else {
			items := make([]any, len(values))
			for i, value := range values {
				items[i] = value
			}
			item["answers"] = items
		}
	default:
		return nil, errUnclearAnswer // Password, approval, planning and form never enter this path.
	}
	if _, err := toolinteraction.NormalizeQuestionSubmitItem(question, item); err != nil {
		return nil, errUnclearAnswer
	}
	return item, nil
}

func parseSpokenOptions(question map[string]any, text string) ([]string, error) {
	options, _ := question["options"].([]any)
	labels := make([]string, len(options))
	for i, raw := range options {
		option, _ := raw.(map[string]any)
		labels[i] = contracts.AnyStringNode(option["label"])
	}
	match := func(part string) (string, bool, error) {
		matches := 0
		label := ""
		for _, value := range labels {
			if part == value {
				matches++
				label = value
			}
		}
		if matches > 1 {
			return "", false, errUnclearAnswer
		}
		if matches == 1 {
			return label, true, nil
		}
		ordinal := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(part, "第"), "个"), "项")
		if n, err := parseSpokenNumber(ordinal); err == nil {
			if n != math.Trunc(n) || n < 1 || n > float64(len(labels)) {
				return "", false, errUnclearAnswer
			}
			return labels[int(n)-1], true, nil
		}
		return "", false, nil
	}
	// A full label wins before splitting words that may be part of its name.
	if label, ok, err := match(text); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return []string{label}, nil
	}
	for _, prefix := range []string{"我选择", "我选", "选择", "选"} {
		if strings.HasPrefix(text, prefix) {
			text = strings.TrimSpace(strings.TrimPrefix(text, prefix))
			break
		}
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "以及", "和"), "还有", "和")
	parts := optionSeparators.Split(text, -1)
	values := []string{}
	seen := map[string]bool{}
	hasOption, implicitFree := false, false
	freeAllowed := contracts.AnyBoolNode(question["allowFreeText"])
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errUnclearAnswer
		}
		value, matched, err := match(part)
		if err != nil {
			return nil, err
		}
		if matched {
			hasOption = true
		} else {
			if !freeAllowed || strings.Contains(part, "第") {
				return nil, errUnclearAnswer
			}
			value = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(part, "其他："), "其他:"))
			if value == part {
				implicitFree = true
			}
			if value == "" {
				return nil, errUnclearAnswer
			}
		}
		if !seen[value] {
			seen[value] = true
			values = append(values, value)
		}
	}
	if hasOption && implicitFree {
		return nil, errUnclearAnswer
	}
	if !hasOption && implicitFree {
		return []string{text}, nil
	}
	return values, nil
}

func parseSpokenNumber(text string) (float64, error) {
	text = strings.TrimSpace(text)
	if arabicNumber.MatchString(text) {
		value, err := strconv.ParseFloat(text, 64)
		if err == nil && !math.IsInf(value, 0) && !math.IsNaN(value) {
			return value, nil
		}
		return 0, errUnclearAnswer
	}
	sign := 1.0
	if strings.HasPrefix(text, "负") || strings.HasPrefix(text, "-") {
		sign = -1
		text = string([]rune(text)[1:])
	} else if strings.HasPrefix(text, "正") || strings.HasPrefix(text, "+") {
		text = string([]rune(text)[1:])
	}
	parts := strings.Split(text, "点")
	if len(parts) > 2 || parts[0] == "" {
		return 0, errUnclearAnswer
	}
	whole, err := chineseInteger(parts[0])
	if err != nil {
		return 0, err
	}
	value := float64(whole)
	if len(parts) == 2 {
		if parts[1] == "" {
			return 0, errUnclearAnswer
		}
		place := 0.1
		for _, r := range parts[1] {
			d, ok := chineseDigit(r)
			if !ok {
				return 0, errUnclearAnswer
			}
			value += float64(d) * place
			place /= 10
		}
	}
	return sign * value, nil
}

func chineseDigit(r rune) (int64, bool) {
	if r >= '0' && r <= '9' {
		return int64(r - '0'), true
	}
	if r == '两' {
		return 2, true
	}
	if r == '〇' {
		return 0, true
	}
	for i, c := range []rune("零一二三四五六七八九") {
		if r == c {
			return int64(i), true
		}
	}
	return 0, false
}

func chineseInteger(text string) (int64, error) {
	if text == "" {
		return 0, errUnclearAnswer
	}
	digitsOnly, value := true, int64(0)
	for _, r := range text {
		d, ok := chineseDigit(r)
		if !ok {
			digitsOnly = false
			break
		}
		if value > 900719925474099 {
			return 0, errUnclearAnswer
		}
		value = value*10 + d
	}
	if digitsOnly {
		return value, nil
	}
	var total, section, digit int64
	lastSmall, lastLarge := int64(10000), int64(1000000000000)
	previousDigit, zeroAfterUnit := false, false
	for _, r := range text {
		if d, ok := chineseDigit(r); ok {
			if previousDigit && digit != 0 {
				return 0, errUnclearAnswer
			}
			digit = d
			previousDigit = true
			if d == 0 {
				zeroAfterUnit = true
			}
			continue
		}
		unit := map[rune]int64{'十': 10, '百': 100, '千': 1000, '万': 10000, '亿': 100000000}[r]
		if unit == 0 {
			return 0, errUnclearAnswer
		}
		if unit < 10000 {
			if unit >= lastSmall {
				return 0, errUnclearAnswer
			}
			if digit == 0 {
				if unit == 10 && section == 0 {
					digit = 1
				} else {
					return 0, errUnclearAnswer
				}
			}
			section += digit * unit
			lastSmall = unit
		} else {
			if unit >= lastLarge || section+digit == 0 {
				return 0, errUnclearAnswer
			}
			total += (section + digit) * unit
			section = 0
			lastSmall = 10000
			lastLarge = unit
		}
		digit = 0
		previousDigit = false
		zeroAfterUnit = false
	}
	if digit != 0 && lastSmall >= 100 && (lastSmall != 10000 || total != 0) && !zeroAfterUnit {
		return 0, errUnclearAnswer
	}
	return total + section + digit, nil
}

func parseSpokenDate(text, kind, zone string, finalized time.Time) (string, error) {
	// Keep existing explicit date/datetime forms valid without inventing a zone.
	if _, err := toolinteraction.NormalizeQuestionSubmitItem(map[string]any{"type": kind}, map[string]any{"answer": text}); err == nil {
		return text, nil
	}
	var year, month, day int
	rest := ""
	matched := false
	for i, prefix := range []string{"今天", "明天", "后天"} {
		if strings.HasPrefix(text, prefix) {
			if zone == "" || zone == "Local" || finalized.IsZero() {
				return "", errUnclearAnswer
			}
			location, err := time.LoadLocation(zone)
			if err != nil {
				return "", errUnclearAnswer
			}
			date := finalized.In(location).AddDate(0, 0, i)
			year, month, day = date.Year(), int(date.Month()), date.Day()
			rest = strings.TrimSpace(strings.TrimPrefix(text, prefix))
			matched = true
			break
		}
	}
	if !matched {
		parts := chineseDate.FindStringSubmatch(text)
		if parts == nil {
			return "", errUnclearAnswer
		}
		values := make([]int, 3)
		for i := range values {
			n, err := chineseInteger(parts[i+1])
			if err != nil {
				return "", err
			}
			values[i] = int(n)
		}
		year, month, day = values[0], values[1], values[2]
		rest = strings.TrimSpace(parts[4])
	}
	date := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
	if kind == "date" {
		if rest != "" {
			return "", errUnclearAnswer
		}
		return date, nil
	}
	period := ""
	for _, prefix := range []string{"上午", "早上", "凌晨", "下午", "晚上", "中午"} {
		if strings.HasPrefix(rest, prefix) {
			period = prefix
			rest = strings.TrimSpace(strings.TrimPrefix(rest, prefix))
			break
		}
	}
	hour, minute, second := int64(-1), int64(0), int64(0)
	if parts := colonClock.FindStringSubmatch(rest); parts != nil {
		hour, _ = strconv.ParseInt(parts[1], 10, 64)
		minute, _ = strconv.ParseInt(parts[2], 10, 64)
		if parts[3] != "" {
			second, _ = strconv.ParseInt(parts[3], 10, 64)
		}
	} else if parts := spokenClock.FindStringSubmatch(rest); parts != nil {
		var err error
		hour, err = chineseInteger(parts[1])
		if err != nil {
			return "", err
		}
		if parts[2] != "" {
			minute = 30
		} else if parts[3] != "" {
			minute, err = chineseInteger(parts[3])
			if err != nil {
				return "", err
			}
		}
		if period == "" && hour < 13 {
			return "", errUnclearAnswer
		}
	} else {
		return "", errUnclearAnswer
	}
	if period != "" {
		if hour < 0 || hour > 12 {
			return "", errUnclearAnswer
		}
		switch period {
		case "下午", "晚上":
			if hour < 12 {
				hour += 12
			}
		case "中午":
			if hour < 11 || hour > 12 {
				return "", errUnclearAnswer
			}
		case "凌晨":
			if hour == 12 {
				hour = 0
			}
			if hour > 5 {
				return "", errUnclearAnswer
			}
		case "上午", "早上":
			if hour == 12 {
				return "", errUnclearAnswer
			}
		}
	}
	if hour < 0 || hour > 23 || minute > 59 || second > 59 {
		return "", errUnclearAnswer
	}
	return fmt.Sprintf("%s %02d:%02d:%02d", date, hour, minute, second), nil
}
