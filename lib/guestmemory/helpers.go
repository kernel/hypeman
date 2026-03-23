package guestmemory

func percentage(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return (float64(part) / float64(total)) * 100
}
