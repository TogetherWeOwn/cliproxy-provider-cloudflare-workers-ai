//go:build !hostcompat

package main

import "context"

func selectedBase(base string) string             { return base }
func configureBase(context.Context, []byte) error { return nil }
