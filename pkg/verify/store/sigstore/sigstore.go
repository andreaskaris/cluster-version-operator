// Package sigstore retrieves signatures using the sig-store protocol
// described in [1].
//
// A URL (scheme http:// or https://) location that contains
// signatures. These signatures are in the atomic container signature
// format. The URL will have the digest of the image appended to it as
// "<STORE>/<ALGO>=<DIGEST>/signature-<NUMBER>" as described in the
// container image signing format. Signatures are searched starting at
// NUMBER 1 and incrementing if the signature exists but is not valid.
//
// [1]: https://github.com/containers/image/blob/ab49b0a48428c623a8f03b41b9083d48966b34a9/docs/signature-protocols.md
package sigstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"k8s.io/klog"

	"github.com/openshift/cluster-version-operator/pkg/verify/store"
)

// maxSignatureSearch prevents unbounded recursion on malicious signature stores (if
// an attacker was able to take ownership of the store to perform DoS on clusters).
const maxSignatureSearch = 10

var errNotFound = errors.New("no more signatures to check")

// Store provides access to signatures stored in memory.
type Store struct {
	// URI is the base from which signature URIs are constructed.
	URI *url.URL

	// HTTPClient is called once for each Signatures call to ensure
	// requests are made with the currently-recommended parameters.
	HTTPClient HTTPClient
}

// Signatures fetches signatures for the provided digest.
func (s *Store) Signatures(ctx context.Context, name string, digest string, fn store.Callback) error {
	parts := strings.SplitN(digest, ":", 3)
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) == 0 {
		return fmt.Errorf("the provided release image digest must be of the form ALGO:HASH")
	}
	algo, hash := parts[0], parts[1]
	equalDigest := fmt.Sprintf("%s=%s", algo, hash)

	switch s.URI.Scheme {
	case "http", "https":
		client, err := s.HTTPClient()
		if err != nil {
			_, err = fn(ctx, nil, err)
			return err
		}

		copied := *s.URI
		copied.Path = path.Join(copied.Path, equalDigest)
		if err := checkHTTPSignatures(ctx, client, copied, maxSignatureSearch, fn); err != nil {
			return err
		}
	default:
		return fmt.Errorf("the store %s scheme is unrecognized", s.URI)
	}

	return nil
}

// checkHTTPSignatures reads signatures as "signature-1", "signature-2", etc. as children of the provided URL
// over HTTP or HTTPS.  No more than maxSignaturesToCheck will be read. If the provided context is cancelled
// search will be terminated.
func checkHTTPSignatures(ctx context.Context, client *http.Client, u url.URL, maxSignaturesToCheck int, fn store.Callback) error {
	base := path.Join(u.Path, "signature-")
	sigURL := u
	for i := 1; i < maxSignatureSearch; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		sigURL.Path = base + strconv.Itoa(i)

		req, err := http.NewRequest("GET", sigURL.String(), nil)
		if err != nil {
			_, err = fn(ctx, nil, fmt.Errorf("could not build request to check signature: %v", err))
			return err // even if the callback ate the error, no sense in checking later indexes which will fail the same way
		}
		req = req.WithContext(ctx)
		// load the body, being careful not to allow unbounded reads
		resp, err := client.Do(req)
		if err != nil {
			klog.V(4).Infof("unable to load signature: %v", err)
			done, err := fn(ctx, nil, err)
			if done || err != nil {
				return err
			}
			continue
		}
		data, err := func() ([]byte, error) {
			body := resp.Body
			r := io.LimitReader(body, 50*1024)

			defer func() {
				// read the remaining body to avoid breaking the connection
				io.Copy(ioutil.Discard, r)
				body.Close()
			}()

			if resp.StatusCode == http.StatusNotFound {
				return nil, errNotFound
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				if i == 1 {
					klog.V(4).Infof("Could not find signature at store location %v", sigURL)
				}
				return nil, fmt.Errorf("unable to retrieve signature from %v: %d", sigURL, resp.StatusCode)
			}

			return ioutil.ReadAll(resp.Body)
		}()
		if err == errNotFound {
			break
		}
		if err != nil {
			klog.V(4).Info(err)
			done, err := fn(ctx, nil, err)
			if done || err != nil {
				return err
			}
			continue
		}
		if len(data) == 0 {
			continue
		}

		done, err := fn(ctx, data, nil)
		if done || err != nil {
			return err
		}
	}
	return nil
}

// String returns a description of where this store finds
// signatures.
func (s *Store) String() string {
	return fmt.Sprintf("containers/image signature store under %s", s.URI)
}
