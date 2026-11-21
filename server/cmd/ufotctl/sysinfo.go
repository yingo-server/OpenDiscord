package main

import "syscall"

// diskFreeBytes returns (available, total) bytes for the filesystem holding
// path. Pure syscall wrapper — injectable through environment.diskFree so the
// doctor thresholds are unit-testable.
func diskFreeBytes(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, nil
}

// humanBytes renders a byte count with one decimal (MiB/GiB).
func humanBytes(n uint64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmtFloat(float64(n)/float64(gib)) + " GiB"
	case n >= mib:
		return fmtFloat(float64(n)/float64(mib)) + " MiB"
	case n >= kib:
		return fmtFloat(float64(n)/float64(kib)) + " KiB"
	default:
		return itoaU(n) + " B"
	}
}

func fmtFloat(f float64) string {
	whole := int64(f)
	frac := int64((f - float64(whole)) * 10)
	if frac < 0 {
		frac = -frac
	}
	return itoaI(whole) + "." + itoaI(frac)
}

func itoaU(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func itoaI(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
