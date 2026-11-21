// Package destinations stores S3-compatible backup destinations and builds
// clients for them.
package destinations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/s3"
	"github.com/monirujjaman-dev/docklet/internal/secrets"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// Service resolves destinations to clients.
type Service struct {
	st  *store.Store
	box *secrets.Box
	// Configure adjusts every client (tests: shorter part size).
	Configure func(*s3.Client)
}

// New returns a service.
func New(st *store.Store, box *secrets.Box) *Service { return &Service{st: st, box: box} }

func aad(id int64) []byte { return []byte(fmt.Sprintf("s3-destination:%d:secret", id)) }

// SealSecret encrypts a destination's secret key, bound to its id.
func (s *Service) SealSecret(id int64, secret string) ([]byte, error) {
	return s.box.Seal([]byte(secret), aad(id))
}

// Input is a destination as entered by the admin.
type Input struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	PathStyle bool   `json:"pathStyle"`
}

// Normalize trims the input and validates it by building a client.
func (in *Input) Normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Endpoint = strings.TrimRight(strings.TrimSpace(in.Endpoint), "/")
	in.Region = strings.TrimSpace(in.Region)
	in.Bucket = strings.TrimSpace(in.Bucket)
	in.Prefix = strings.Trim(strings.TrimSpace(in.Prefix), "/")
	in.AccessKey = strings.TrimSpace(in.AccessKey)
	if in.Name == "" || len(in.Name) > 64 {
		return errors.New("name is required (max 64 characters)")
	}
	if len(in.Prefix) > 200 || strings.Contains(in.Prefix, "..") {
		return errors.New("prefix must be a plain path of at most 200 characters")
	}
	_, err := s3.New(in.config())
	return err
}

func (in Input) config() s3.Config {
	return s3.Config{Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket, Prefix: in.Prefix,
		AccessKey: in.AccessKey, SecretKey: in.SecretKey, PathStyle: in.PathStyle}
}

// ClientFor builds a client from unsaved input (for "test before save").
func (s *Service) ClientFor(in Input) (*s3.Client, error) {
	c, err := s3.New(in.config())
	if err != nil {
		return nil, err
	}
	if s.Configure != nil {
		s.Configure(c)
	}
	return c, nil
}

// Client returns the stored destination and a client for it.
func (s *Service) Client(ctx context.Context, id int64) (*s3.Client, db.S3Destination, error) {
	d, err := s.st.Q.GetDestination(ctx, id)
	if err != nil {
		return nil, d, fmt.Errorf("backup destination %d: %w", id, err)
	}
	secret, err := s.box.Open(d.SecretEnc, aad(id))
	if err != nil {
		return nil, d, fmt.Errorf("decrypt the secret of destination %q: %w", d.Name, err)
	}
	c, err := s.ClientFor(Input{Endpoint: d.Endpoint, Region: d.Region, Bucket: d.Bucket, Prefix: d.Prefix,
		AccessKey: d.AccessKey, SecretKey: string(secret), PathStyle: d.PathStyle == 1})
	return c, d, err
}

// Test checks a stored destination and records the outcome.
func (s *Service) Test(ctx context.Context, id int64) error {
	c, _, err := s.Client(ctx, id)
	if err == nil {
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = c.Test(tctx)
		cancel()
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = s.st.Q.SetDestinationTest(context.WithoutCancel(ctx), db.SetDestinationTestParams{LastTestAt: time.Now().Unix(), LastTestError: msg, ID: id})
	return err
}
