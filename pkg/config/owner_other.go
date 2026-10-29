//go:build !unix

package config

import "os"

func preserveOwner(*os.File, os.FileInfo) error { return nil }
