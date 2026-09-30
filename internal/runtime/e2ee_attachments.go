package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxd/internal/safehttp"
)

// Attachment descriptors are consumed here, after envelope authentication and
// before ACP dispatch. Neither download capabilities nor crypto context reach
// the agent, ordinary control channel, or plaintext history.
func localizeE2EEAttachments(ctx context.Context, payload []byte, rootKey []byte, envelope e2ee.Envelope) ([]byte, error) {
	var frame map[string]json.RawMessage
	if json.Unmarshal(payload, &frame) != nil {
		return payload, nil
	}
	var method string
	_ = json.Unmarshal(frame["method"], &method)
	if method != "session/prompt" {
		return payload, nil
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(frame["params"], &params); err != nil {
		return nil, err
	}
	var attachments []e2ee.Attachment
	if err := json.Unmarshal(params["paxEncryptedAttachments"], &attachments); err != nil && params["paxEncryptedAttachments"] != nil {
		return nil, errors.New("invalid encrypted attachments")
	}
	if len(attachments) == 0 {
		return payload, nil
	}
	if len(attachments) > 16 {
		return nil, errors.New("too many encrypted attachments")
	}
	var prompt []json.RawMessage
	if err := json.Unmarshal(params["prompt"], &prompt); err != nil {
		return nil, err
	}
	dirs := []string{}
	success := false
	defer func() {
		if !success {
			for _, dir := range dirs {
				_ = os.RemoveAll(dir)
			}
		}
	}()
	for _, attachment := range attachments {
		if attachment.AgentID != envelope.AgentID || attachment.SessionID != envelope.SessionID || attachment.KeyEpoch != envelope.KeyEpoch {
			return nil, errors.New("encrypted attachment route mismatch")
		}
		if err := attachment.Validate(); err != nil {
			return nil, err
		}
		path, err := downloadE2EEAttachment(ctx, rootKey, attachment)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, filepath.Dir(path))
		block, err := json.Marshal(map[string]any{"type": "resource_link", "uri": (&url.URL{Scheme: "file", Path: path}).String(), "name": attachment.Filename, "mimeType": attachment.ContentType})
		if err != nil {
			return nil, err
		}
		prompt = append(prompt, block)
	}
	params["prompt"], _ = json.Marshal(prompt)
	delete(params, "paxEncryptedAttachments")
	frame["params"], _ = json.Marshal(params)
	result, err := json.Marshal(frame)
	success = err == nil
	return result, err
}

func downloadE2EEAttachment(ctx context.Context, rootKey []byte, attachment e2ee.Attachment) (string, error) {
	parsed, err := url.Parse(attachment.DownloadURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("encrypted attachment requires an HTTPS download URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, attachment.DownloadURL, nil)
	if err != nil {
		return "", errors.New("invalid attachment download URL")
	}
	response, err := safehttp.DoNoRedirect(http.DefaultClient, req)
	if err != nil {
		return "", safehttp.RedactError("encrypted attachment download", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("encrypted attachment download returned %d; resend to refresh the ticket", response.StatusCode)
	}
	if response.ContentLength >= 0 && response.ContentLength != attachment.CiphertextSize() {
		return "", errors.New("encrypted attachment size mismatch")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	scope := sha256.Sum256([]byte(attachment.AgentID + "\x00" + attachment.SessionID))
	root := filepath.Join(home, ".paxd", "e2ee-attachments", hex.EncodeToString(scope[:]))
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "file-")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dir)
		}
	}()
	file, err := os.OpenFile(filepath.Join(dir, ".partial"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := e2ee.DecryptAttachment(rootKey, attachment, response.Body, file); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	name := filepath.Base(strings.ReplaceAll(attachment.Filename, "\\", "/"))
	if name == "" || name == "." || name == ".." || name == ".partial" {
		name = "attachment"
	}
	path := filepath.Join(dir, name)
	if err := os.Rename(filepath.Join(dir, ".partial"), path); err != nil {
		return "", err
	}
	success = true
	return path, nil
}
