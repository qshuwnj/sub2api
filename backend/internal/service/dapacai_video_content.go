package service

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var dapacaiContentPath = regexp.MustCompile(`^/video/[0-9]{4}/(0[1-9]|1[0-2])/task_[A-Za-z0-9_-]+\.mp4$`)

func dapacaiContentRedirectURL(raw, task string) (string, error) {
	id, ok := strings.CutPrefix(task, "dapacai:")
	u, err := url.Parse(raw)
	if !ok || !dapacaiTaskID.MatchString(id) || err != nil || u.Scheme != "https" ||
		u.Host != "dapacaihk-media.oss-cn-hongkong.aliyuncs.com" ||
		u.User != nil || u.Fragment != "" || u.RawPath != "" ||
		!dapacaiContentPath.MatchString(u.Path) || !strings.HasSuffix(u.Path, "/"+id+".mp4") {
		return "", errors.New("Dapacai returned an unsupported video download destination")
	}
	return u.String(), nil
}

func dapacaiVideoContentType(raw string) bool {
	mediaType, _, err := mime.ParseMediaType(raw)
	return err == nil && (strings.HasPrefix(mediaType, "video/") || mediaType == "application/octet-stream")
}

// Only the verified task-specific OSS destination is followed, without supplier
// credentials. Keep automatic redirects disabled and reject private DNS results.
// Direct downloads and native-content archival share this security boundary.
func (s *OpenAIGatewayService) followDapacaiContentRedirect(ctx context.Context, account *Account, task, proxy, byteRange string, resp *http.Response) (*http.Response, error) {
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return resp, nil
	}
	target, err := dapacaiContentRedirectURL(resp.Header.Get("Location"), task)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	ctx = WithHTTPUpstreamPublicHostsOnly(WithHTTPUpstreamRedirectsDisabled(ctx))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("Dapacai video download request is invalid")
	}
	req.Header.Set("Accept", "video/*, application/octet-stream")
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	resp, err = s.httpUpstream.Do(req, proxy, account.ID, account.Concurrency)
	if err != nil {
		// Do not expose signed URLs in wrapped transport errors.
		return nil, errors.New("Dapacai video download interrupted")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, errors.New("Dapacai video download redirected more than once")
	}
	return resp, nil
}
