//go:build !linux

package utils

func SetAutoStart(enable bool) error { return nil }
