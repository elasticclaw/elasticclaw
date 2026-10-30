//go:build !linux && !darwin

package install

func setUmask(int) func() { return func() {} }
