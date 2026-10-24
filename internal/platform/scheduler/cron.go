package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronSchedule is a parsed 5-field cron expression evaluated in UTC.
// Fields are minute, hour, day-of-month, month, day-of-week.
type cronSchedule struct {
	minute  uint64
	hour    uint64
	dom     uint64
	month   uint64
	dow     uint64
	domStar bool
	dowStar bool
}

var cronAliases = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

func parseCron(expr string) (*cronSchedule, error) {
	expr = strings.ToLower(strings.TrimSpace(expr))
	if expr == "" {
		return nil, fmt.Errorf("cron: expression is required")
	}
	if strings.HasPrefix(expr, "@") {
		expanded, ok := cronAliases[expr]
		if !ok {
			return nil, fmt.Errorf("cron: unknown descriptor %q", expr)
		}
		expr = expanded
	}

	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields, got %d", len(fields))
	}

	minute, _, err := parseCronField(fields[0], 0, 59, false)
	if err != nil {
		return nil, fmt.Errorf("cron: minute: %w", err)
	}
	hour, _, err := parseCronField(fields[1], 0, 23, false)
	if err != nil {
		return nil, fmt.Errorf("cron: hour: %w", err)
	}
	dom, domStar, err := parseCronField(fields[2], 1, 31, false)
	if err != nil {
		return nil, fmt.Errorf("cron: day of month: %w", err)
	}
	month, _, err := parseCronField(fields[3], 1, 12, false)
	if err != nil {
		return nil, fmt.Errorf("cron: month: %w", err)
	}
	dow, dowStar, err := parseCronField(fields[4], 0, 7, true)
	if err != nil {
		return nil, fmt.Errorf("cron: day of week: %w", err)
	}

	return &cronSchedule{
		minute:  minute,
		hour:    hour,
		dom:     dom,
		month:   month,
		dow:     dow,
		domStar: domStar,
		dowStar: dowStar,
	}, nil
}

// parseCronField parses lists, ranges, and steps into a bitmask. allowSunday7
// maps day-of-week 7 to Sunday, matching common cron implementations.
func parseCronField(field string, minValue, maxValue int, allowSunday7 bool) (mask uint64, star bool, err error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return 0, false, fmt.Errorf("empty field")
	}
	star = field == "*"

	for _, item := range strings.Split(field, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return 0, false, fmt.Errorf("empty list item")
		}
		lo, hi, step, err := parseCronItem(item, minValue, maxValue)
		if err != nil {
			return 0, false, err
		}
		for value := lo; value <= hi; value += step {
			bit := value
			if allowSunday7 && bit == 7 {
				bit = 0
			}
			mask |= 1 << uint(bit)
		}
	}
	return mask, star, nil
}

// parseCronItem parses one list item ("*", "N", "A-B", each optionally
// "/step") into the inclusive range it selects and its step.
func parseCronItem(item string, minValue, maxValue int) (lo, hi, step int, err error) {
	base, step, err := splitCronStep(item)
	if err != nil {
		return 0, 0, 0, err
	}

	switch {
	case base == "*":
		// Full range, optionally stepped.
		lo, hi = minValue, maxValue
	case strings.Contains(base, "-"):
		lo, hi, err = parseCronRange(base)
		if err != nil {
			return 0, 0, 0, err
		}
	default:
		value, convErr := strconv.Atoi(base)
		if convErr != nil {
			return 0, 0, 0, fmt.Errorf("invalid value %q", base)
		}
		lo, hi = value, value
		if step > 1 {
			// Vixie cron treats "N/step" as N through the field maximum.
			hi = maxValue
		}
	}

	if lo < minValue || lo > maxValue || hi < minValue || hi > maxValue {
		return 0, 0, 0, fmt.Errorf("value out of range %d-%d in %q", minValue, maxValue, item)
	}
	return lo, hi, step, nil
}

// splitCronStep separates "base/step"; an item without a step has step 1.
func splitCronStep(item string) (base string, step int, err error) {
	before, after, found := strings.Cut(item, "/")
	if !found {
		return item, 1, nil
	}
	step, err = strconv.Atoi(strings.TrimSpace(after))
	base = strings.TrimSpace(before)
	if err != nil || step <= 0 || base == "" {
		return "", 0, fmt.Errorf("invalid step %q", item)
	}
	return base, step, nil
}

// parseCronRange parses an inclusive "start-end" range.
func parseCronRange(base string) (start, end int, err error) {
	left, right, _ := strings.Cut(base, "-")
	start, err = strconv.Atoi(strings.TrimSpace(left))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range start %q", left)
	}
	end, err = strconv.Atoi(strings.TrimSpace(right))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range end %q", right)
	}
	if start > end {
		return 0, 0, fmt.Errorf("range start %d is after end %d", start, end)
	}
	return start, end, nil
}

// Next returns the first occurrence strictly after t.
func (c *cronSchedule) Next(t time.Time) (time.Time, bool) {
	candidate := t.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(10, 0, 0)

	for candidate.Before(limit) {
		switch {
		case !c.monthMatches(candidate):
			candidate = time.Date(candidate.Year(), candidate.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !c.dayMatches(candidate):
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day()+1, 0, 0, 0, 0, time.UTC)
		case !bitSet(c.hour, candidate.Hour()):
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), candidate.Hour()+1, 0, 0, 0, time.UTC)
		case !bitSet(c.minute, candidate.Minute()):
			candidate = candidate.Add(time.Minute)
		default:
			return candidate, true
		}
	}
	return time.Time{}, false
}

// Prev returns the last occurrence strictly before t. It powers catch-up
// calculations, including "what was the latest slot at or before now".
func (c *cronSchedule) Prev(t time.Time) (time.Time, bool) {
	candidate := t.UTC().Truncate(time.Minute)
	if candidate.Equal(t) {
		candidate = candidate.Add(-time.Minute)
	}
	limit := candidate.AddDate(-10, 0, 0)

	for candidate.After(limit) {
		switch {
		case !c.monthMatches(candidate):
			candidate = time.Date(candidate.Year(), candidate.Month(), 1, 0, 0, 0, 0, time.UTC).Add(-time.Minute)
		case !c.dayMatches(candidate):
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), 0, 0, 0, 0, time.UTC).Add(-time.Minute)
		case !bitSet(c.hour, candidate.Hour()):
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(), candidate.Hour(), 0, 0, 0, time.UTC).Add(-time.Minute)
		case !bitSet(c.minute, candidate.Minute()):
			candidate = candidate.Add(-time.Minute)
		default:
			return candidate, true
		}
	}
	return time.Time{}, false
}

// latestDue returns the latest occurrence at or before now.
func (c *cronSchedule) latestDue(now time.Time) (time.Time, bool) {
	return c.Prev(now.UTC().Truncate(time.Minute).Add(time.Minute))
}

func (c *cronSchedule) monthMatches(t time.Time) bool {
	return bitSet(c.month, int(t.Month()))
}

func (c *cronSchedule) dayMatches(t time.Time) bool {
	domMatch := bitSet(c.dom, t.Day())
	dowMatch := bitSet(c.dow, int(t.Weekday()))
	// Standard cron ORs day-of-month and day-of-week when both are
	// restricted, and ANDs when either is a bare wildcard.
	if c.domStar || c.dowStar {
		return domMatch && dowMatch
	}
	return domMatch || dowMatch
}

func bitSet(mask uint64, bit int) bool {
	if bit < 0 || bit > 63 {
		return false
	}
	return mask&(1<<uint(bit)) != 0
}
