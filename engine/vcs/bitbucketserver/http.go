package bitbucketserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/rockbears/log"

	"github.com/ovh/cds/engine/cache"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/cdsclient"
	"github.com/ovh/cds/sdk/telemetry"
)

var (
	httpClient = cdsclient.NewHTTPClient(time.Second*30, false)
)

func requestString(method string, uri string, params map[string]string) string {
	// loop through params, add keys to map
	var keys []string
	for key := range params {
		keys = append(keys, key)
	}

	// sort the array of header keys
	sort.StringSlice(keys).Sort()

	// create the signed string
	result := method + "&" + escape(uri)

	// loop through sorted params and append to the string
	for pos, key := range keys {
		if pos == 0 {
			result += "&"
		} else {
			result += escape("&")
		}

		result += escape(fmt.Sprintf("%s=%s", key, escape(params[key])))
	}

	return result
}

func (c *bitbucketClient) getFullAPIURL(api string) string {
	var url string
	switch api {
	case "keys":
		url = fmt.Sprintf("%s/rest/keys/1.0", c.consumer.URL)
	case "ssh":
		url = fmt.Sprintf("%s/rest/ssh/1.0", c.consumer.URL)
	case "core":
		url = fmt.Sprintf("%s/rest/api/1.0", c.consumer.URL)
	case "build-status":
		url = fmt.Sprintf("%s/rest/build-status/1.0", c.consumer.URL)
	}

	return url
}

type options struct {
	asUser bool
}

func (c *bitbucketClient) do(ctx context.Context, method, api, path string, params url.Values, values []byte, v interface{}, opts *options) error {
	ctx, end := telemetry.Span(ctx, "bitbucketserver.do_http")
	defer end()

	// Sad hack to get username
	var username = false
	if path == "username" {
		username = true
		path = "/repos"
	}

	// create the URI
	apiURL := c.getFullAPIURL(api)
	uri, err := url.Parse(apiURL + path)
	if err != nil {
		return sdk.WithStack(err)
	}

	if params != nil && len(params) > 0 {
		uri.RawQuery = params.Encode()
	}

	// create the access token
	token := NewAccessToken(c.accessToken, c.accessTokenSecret, nil)

	// create the request
	req := &http.Request{
		URL:        uri,
		Method:     method,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Close:      true,
		Header:     http.Header{},
	}

	log.Info(ctx, "%s %s", req.Method, req.URL.String())

	if values != nil && len(values) > 0 {
		buf := bytes.NewBuffer(values)
		req.Body = ioutil.NopCloser(buf)
		req.ContentLength = int64(buf.Len())
	}

	// sign the request
	if opts != nil && opts.asUser && c.token != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))
	} else {
		if err := c.consumer.Sign(req, token); err != nil {
			return err
		}
	}

	// ensure the appropriate content-type is set for POST,
	// assuming the field is not populated
	if (req.Method == "POST" || req.Method == "PUT") && len(req.Header.Get("Content-Type")) == 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	cacheKey := cache.Key("vcs", "bitbucket", "request", req.URL.String(), token.Token())
	if v != nil && method == "GET" {
		find, err := c.consumer.cache.Get(cacheKey, v)
		if err != nil {
			log.Error(ctx, "cannot get from cache %s: %v", cacheKey, err)
		}
		if find {
			return nil
		}
	}

	// make the request using the default http client
	resp, err := httpClient.Do(req)
	if err != nil {
		return sdk.WrapError(err, "HTTP Error")
	}

	// Read the bytes from the body (make sure we defer close the body)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sdk.WithStack(err)
	}

	// Check for an http error status (ie not 200 StatusOK)
	switch resp.StatusCode {
	case 404:
		return sdk.WithStack(sdk.ErrNotFound)
	case 403:
		return sdk.WithStack(sdk.ErrForbidden)
	case 401:
		var debugAT, debugAS string
		if len(c.accessToken) > 4 {
			debugAT = c.accessToken[:4]
		}
		if len(c.accessTokenSecret) > 4 {
			debugAS = c.accessTokenSecret[:4]
		}
		log.Info(ctx, "debug_auth accessToken_http lenat:%d lenas:%d at:%v as:%v", len(c.accessToken), len(c.accessTokenSecret), debugAT, debugAS)
		return sdk.WithStack(sdk.ErrUnauthorized)
	case 400:
		log.Warn(ctx, "bitbucketClient.do> %s", string(body))
		return sdk.WithStack(sdk.ErrWrongRequest)
	}

	if method != "GET" {
		if err := c.consumer.cache.Delete(cacheKey); err != nil {
			log.Error(ctx, "bitbucketClient.do> unable to delete cache key %v: %v", cacheKey, err)
		}
	}

	// Unmarshall the JSON response
	if v != nil {
		// If looking for username then pull that from header
		if username {
			body, err = json.Marshal(map[string]string{"name": resp.Header["X-Ausername"][0]})
			if err != nil {
				return nil
			}
		}

		// bitbucket can return 204 with no-content
		if resp.StatusCode != 204 || strings.TrimSpace(string(body)) != "" {
			if err := sdk.JSONUnmarshal(body, v); err != nil {
				return err
			}
		}
		if method == "GET" {
			if err := c.consumer.cache.Set(cacheKey, v); err != nil {
				log.Error(ctx, "unable to cache set %v: %v", cacheKey, err)
			}
		}
	}

	return nil
}
