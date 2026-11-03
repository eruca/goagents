package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Times are stored as INTEGER Unix nanoseconds, with 0 for the zero time, so
// SQL comparisons and ORDER BY follow the instant. The driver's default text
// encoding includes the zone name and compares as a string, which orders
// local and UTC times wrongly.
func dbTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

type timeColumn struct {
	dst *time.Time
}

func scanTime(dst *time.Time) timeColumn {
	return timeColumn{dst: dst}
}

func (c timeColumn) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*c.dst = time.Time{}
	case int64:
		if v == 0 {
			*c.dst = time.Time{}
		} else {
			*c.dst = time.Unix(0, v).UTC()
		}
	case time.Time:
		// A row written before times were stored as integers.
		*c.dst = v.UTC()
	case string:
		parsed, err := parseLegacyTime(v)
		if err != nil {
			return err
		}
		*c.dst = parsed
	default:
		return fmt.Errorf("unsupported time column value %T", src)
	}
	return nil
}

// parseLegacyTime reads the driver's text encoding (time.Time.String, whose
// trailing zone name the driver cannot always parse back) or RFC 3339. Only
// the date, clock, and numeric offset matter.
func parseLegacyTime(value string) (time.Time, error) {
	candidates := []struct{ layout, value string }{{time.RFC3339Nano, value}}
	if fields := strings.Fields(value); len(fields) >= 3 {
		candidates = append(candidates, struct{ layout, value string }{"2006-01-02 15:04:05.999999999 -0700", strings.Join(fields[:3], " ")})
	}
	for _, candidate := range candidates {
		if parsed, err := time.Parse(candidate.layout, candidate.value); err == nil {
			if parsed.IsZero() {
				return time.Time{}, nil
			}
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", value)
}

// convertTextTimes rewrites time values left in the driver's text encoding by
// an older schema as integers.
func convertTextTimes(ctx context.Context, db *sql.DB, table string, columns ...string) error {
	for _, column := range columns {
		rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT rowid, %s FROM %s WHERE typeof(%s) = 'text'`, column, table, column))
		if err != nil {
			return err
		}
		type legacy struct {
			rowid int64
			value time.Time
		}
		var values []legacy
		for rows.Next() {
			var row legacy
			if err := rows.Scan(&row.rowid, scanTime(&row.value)); err != nil {
				_ = rows.Close()
				return fmt.Errorf("convert %s.%s: %w", table, column, err)
			}
			values = append(values, row)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, row := range values {
			if _, err := db.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, table, column), dbTime(row.value), row.rowid); err != nil {
				return err
			}
		}
	}
	return nil
}
