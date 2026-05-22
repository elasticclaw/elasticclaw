package cmd

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
