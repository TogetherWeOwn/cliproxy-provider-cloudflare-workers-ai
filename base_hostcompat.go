//go:build hostcompat

package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

var testBase atomic.Value

func configureBase(_ context.Context, raw []byte) error {
	var cfg struct {
		Base string `yaml:"hostcompat_base"`
	}
	if yaml.Unmarshal(raw, &cfg) != nil {
		return errors.New("invalid test configuration")
	}
	u, err := url.Parse(cfg.Base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		return errors.New("test upstream must be loopback")
	}
	testBase.Store(cfg.Base)
	return nil
}

// Host compatibility binaries are test-only and refuse non-loopback upstreams.
func selectedBase(_ string) string {
	if base := testBase.Load(); base != nil {
		return base.(string)
	}
	return "http://127.0.0.1:0"
}
