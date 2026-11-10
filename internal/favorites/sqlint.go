package favorites

type sqlInt int64

// Scan implements sql.Scanner for the int64 post_id/saved_at columns.
func (s *sqlInt) Scan(v any) error {
	switch t := v.(type) {
	case int64:
		*s = sqlInt(t)
	case float64:
		*s = sqlInt(int64(t))
	case []byte:
		*s = sqlInt(parseBytes(t))
	case string:
		*s = sqlInt(parseBytes([]byte(t)))
	case nil:
		*s = 0
	}
	return nil
}

func parseBytes(b []byte) int64 {
	n := int64(0)
	neg := false
	i := 0
	if len(b) > 0 && (b[0] == '-' || b[0] == '+') {
		neg = b[0] == '-'
		i = 1
	}
	for ; i < len(b); i++ {
		if b[i] < '0' || b[i] > '9' {
			break
		}
		n = n*10 + int64(b[i]-'0')
	}
	if neg {
		return -n
	}
	return n
}
