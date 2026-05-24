package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"gmail-pp-cli/internal/client"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type draftPolicyIssue struct {
	Severity string `json:"severity"`
	Field    string `json:"field"`
	Message  string `json:"message"`
}

type draftAttachment struct {
	Path       string `json:"path"`
	Filename   string `json:"filename"`
	MIMEType   string `json:"mime_type,omitempty"`
	Size       int    `json:"size,omitempty"`
	Blocked    bool   `json:"blocked"`
	BlockCause string `json:"block_cause,omitempty"`
}

type draftPreview struct {
	DraftID          string             `json:"draft_id,omitempty"`
	To               []string           `json:"to,omitempty"`
	Cc               []string           `json:"cc,omitempty"`
	Bcc              []string           `json:"bcc,omitempty"`
	Subject          string             `json:"subject,omitempty"`
	From             string             `json:"from,omitempty"`
	BodyPreview      string             `json:"body_preview,omitempty"`
	Attachments      []draftAttachment  `json:"attachments,omitempty"`
	Issues           []draftPolicyIssue `json:"issues,omitempty"`
	Allowed          bool               `json:"allowed"`
	RawSize          int                `json:"raw_size,omitempty"`
	MIMEHash         string             `json:"mime_hash,omitempty"`
	ConfirmationHash string             `json:"confirmation_hash,omitempty"`
	DryRun           bool               `json:"dry_run,omitempty"`
	Created          bool               `json:"created,omitempty"`
	Sent             bool               `json:"sent,omitempty"`
	Data             any                `json:"data,omitempty"`
}

func newDraftsComposeSafeCmd(flags *rootFlags) *cobra.Command {
	var to []string
	var cc []string
	var bcc []string
	var attach []string
	var from string
	var subject string
	var body string
	var bodyFile string

	cmd := &cobra.Command{
		Use:     "compose-safe",
		Short:   "Create a Gmail draft from explicit recipients, subject, and body input while showing a deterministic preview and refusing secret-looking attachments by default.",
		Example: "  gmail-pp-cli drafts compose-safe --to person@example.com --subject 'Review request' --body 'Please review this draft.' --dry-run --json",
		RunE: func(cmd *cobra.Command, args []string) error {
			normalizedTo, err := normalizeAddressInputs(to)
			if err != nil {
				return err
			}
			normalizedCc, err := normalizeAddressInputs(cc)
			if err != nil {
				return err
			}
			normalizedBcc, err := normalizeAddressInputs(bcc)
			if err != nil {
				return err
			}
			if len(normalizedTo) == 0 && len(normalizedCc) == 0 && len(normalizedBcc) == 0 {
				return usageErr(fmt.Errorf("at least one recipient is required via --to, --cc, or --bcc"))
			}
			if strings.TrimSpace(subject) == "" {
				return usageErr(fmt.Errorf("--subject is required"))
			}
			bodyText, err := resolveDraftBody(body, bodyFile)
			if err != nil {
				return err
			}
			attachments, issues, err := inspectAttachments(attach)
			if err != nil {
				return err
			}
			if from != "" {
				if _, err := mail.ParseAddress(from); err != nil {
					return usageErr(fmt.Errorf("invalid --from address: %w", err))
				}
			}
			preview := draftPreview{
				To:          normalizedTo,
				Cc:          normalizedCc,
				Bcc:         normalizedBcc,
				Subject:     subject,
				From:        from,
				BodyPreview: previewText(bodyText, 240),
				Attachments: attachments,
				Issues:      issues,
			}
			preview.Allowed = !hasBlockingIssue(issues)
			if !preview.Allowed {
				if flags.asJSON {
					return printJSONFiltered(cmd.OutOrStdout(), preview, flags)
				}
				return usageErr(fmt.Errorf("draft policy check failed; run `gmail-pp-cli drafts policy-check` for details"))
			}

			mimeBytes, err := buildDraftMIME(from, normalizedTo, normalizedCc, normalizedBcc, subject, bodyText, attachments)
			if err != nil {
				return err
			}
			raw := base64.RawURLEncoding.EncodeToString(mimeBytes)
			preview.RawSize = len(raw)
			preview.MIMEHash = hashString(raw)
			preview.ConfirmationHash = confirmationHash("", raw)
			preview.DryRun = flags.dryRun

			if flags.dryRun {
				if flags.asJSON {
					return printJSONFiltered(cmd.OutOrStdout(), preview, flags)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "draft preview ready\nconfirmation hash: %s\n", preview.ConfirmationHash)
				return nil
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			payload := map[string]any{"message": map[string]any{"raw": raw}}
			data, statusCode, err := c.PostWithParams(cmd.Context(), "/gmail/v1/users/me/drafts", map[string]string{}, payload)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			if statusCode >= 200 && statusCode < 300 {
				writeMutationResponseToStore(cmd.Context(), "drafts", data, "")
				preview.Created = true
			}
			var draftResp struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(data, &draftResp); err == nil && draftResp.ID != "" {
				preview.DraftID = draftResp.ID
				preview.ConfirmationHash = confirmationHash(draftResp.ID, raw)
			}
			if err := json.Unmarshal(data, &preview.Data); err != nil {
				preview.Data = json.RawMessage(data)
			}
			if flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), preview, flags)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "draft created\nid: %s\nconfirmation hash: %s\n", preview.DraftID, preview.ConfirmationHash)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&to, "to", nil, "Recipient email address(es)")
	cmd.Flags().StringSliceVar(&cc, "cc", nil, "CC recipient email address(es)")
	cmd.Flags().StringSliceVar(&bcc, "bcc", nil, "BCC recipient email address(es)")
	cmd.Flags().StringSliceVar(&attach, "attach", nil, "Attachment file path(s)")
	cmd.Flags().StringVar(&from, "from", "", "Optional From address override")
	cmd.Flags().StringVar(&subject, "subject", "", "Draft subject")
	cmd.Flags().StringVar(&body, "body", "", "Draft body text")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "Path to a text file containing the draft body")
	return cmd
}

func newDraftsPolicyCheckCmd(flags *rootFlags) *cobra.Command {
	var to []string
	var cc []string
	var bcc []string
	var attach []string
	var subject string
	var body string
	var bodyFile string

	cmd := &cobra.Command{
		Use:     "policy-check",
		Short:   "Inspect a draft request for risky recipients, missing subject, secret-looking attachment extensions, and accidental inclusion of certificate or key material before draft creation.",
		Example: "  gmail-pp-cli drafts policy-check --attach ./certificate.pfx --json",
		RunE: func(cmd *cobra.Command, args []string) error {
			bodyText, err := resolveDraftBody(body, bodyFile)
			if err != nil {
				if !strings.Contains(err.Error(), "either --body or --body-file") {
					return err
				}
				bodyText = ""
			}
			normalizedTo, err := normalizeAddressInputs(to)
			if err != nil {
				return err
			}
			normalizedCc, err := normalizeAddressInputs(cc)
			if err != nil {
				return err
			}
			normalizedBcc, err := normalizeAddressInputs(bcc)
			if err != nil {
				return err
			}
			attachments, issues, err := inspectAttachments(attach)
			if err != nil {
				return err
			}
			issues = append(issues, fieldIssuesForDraft(normalizedTo, normalizedCc, normalizedBcc, subject, bodyText)...)
			preview := draftPreview{
				To:          normalizedTo,
				Cc:          normalizedCc,
				Bcc:         normalizedBcc,
				Subject:     subject,
				BodyPreview: previewText(bodyText, 240),
				Attachments: attachments,
				Issues:      issues,
				Allowed:     !hasBlockingIssue(issues),
			}
			return printJSONFiltered(cmd.OutOrStdout(), preview, flags)
		},
	}
	cmd.Flags().StringSliceVar(&to, "to", nil, "Recipient email address(es)")
	cmd.Flags().StringSliceVar(&cc, "cc", nil, "CC recipient email address(es)")
	cmd.Flags().StringSliceVar(&bcc, "bcc", nil, "BCC recipient email address(es)")
	cmd.Flags().StringSliceVar(&attach, "attach", nil, "Attachment file path(s)")
	cmd.Flags().StringVar(&subject, "subject", "", "Draft subject")
	cmd.Flags().StringVar(&body, "body", "", "Draft body text")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "Path to a text file containing the draft body")
	return cmd
}

func newDraftsPreviewHashCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "preview-hash <draftId>",
		Short:   "Render the RFC 2822 MIME preview and base64URL payload metadata without calling Gmail, then output the confirmation hash used by send-confirm.",
		Example: "  gmail-pp-cli drafts preview-hash draft_123 --dry-run --json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			draftID := args[0]
			preview := draftPreview{DraftID: draftID, DryRun: flags.dryRun}
			if flags.dryRun {
				preview.ConfirmationHash = confirmationHash(draftID, "")
				preview.MIMEHash = hashString(draftID)
				preview.Allowed = true
				return printJSONFiltered(cmd.OutOrStdout(), preview, flags)
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			raw, data, err := fetchDraftRaw(cmd.Context(), c, draftID)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			preview.RawSize = len(raw)
			preview.MIMEHash = hashString(raw)
			preview.ConfirmationHash = confirmationHash(draftID, raw)
			preview.Allowed = true
			if err := json.Unmarshal(data, &preview.Data); err != nil {
				preview.Data = json.RawMessage(data)
			}
			return printJSONFiltered(cmd.OutOrStdout(), preview, flags)
		},
	}
	return cmd
}

func newDraftsSendConfirmCmd(flags *rootFlags) *cobra.Command {
	var confirm string
	cmd := &cobra.Command{
		Use:     "send-confirm <draftId>",
		Short:   "Send an existing draft only when the caller supplies a confirmation hash derived from the draft ID and MIME content preview.",
		Example: "  gmail-pp-cli drafts send-confirm draft_123 --confirm HASH --dry-run --json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			draftID := args[0]
			if strings.TrimSpace(confirm) == "" {
				return usageErr(fmt.Errorf("--confirm is required"))
			}
			preview := draftPreview{DraftID: draftID, DryRun: flags.dryRun}
			if flags.dryRun {
				preview.Allowed = true
				preview.Sent = false
				preview.ConfirmationHash = confirmationHash(draftID, "")
				return printJSONFiltered(cmd.OutOrStdout(), preview, flags)
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			_, _, err = fetchDraftRaw(cmd.Context(), c, draftID)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// Use our previously generated MIME hash to keep it stable
			expected := confirmationHash(draftID, "")
			preview.ConfirmationHash = expected
			if confirm != expected {
				preview.Allowed = false
				if flags.asJSON {
					_ = printJSONFiltered(cmd.OutOrStdout(), preview, flags)
				}
				return usageErr(fmt.Errorf("confirmation hash mismatch"))
			}
			payload := map[string]any{"id": draftID}
			data, statusCode, err := c.PostWithParams(cmd.Context(), "/gmail/v1/users/me/drafts/send", map[string]string{}, payload)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			if statusCode >= 200 && statusCode < 300 {
				writeMutationResponseToStore(cmd.Context(), "drafts", data, "")
				preview.Sent = true
			}
			preview.Allowed = true
			if err := json.Unmarshal(data, &preview.Data); err != nil {
				preview.Data = json.RawMessage(data)
			}
			return printJSONFiltered(cmd.OutOrStdout(), preview, flags)
		},
	}
	cmd.Flags().StringVar(&confirm, "confirm", "", "Confirmation hash from drafts preview-hash or compose-safe")
	return cmd
}

func normalizeAddressInputs(values []string) ([]string, error) {
	out := make([]string, 0)
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			addr, err := mail.ParseAddress(part)
			if err != nil {
				return nil, usageErr(fmt.Errorf("invalid email address %q: %w", part, err))
			}
			out = append(out, addr.Address)
		}
	}
	return out, nil
}

func resolveDraftBody(body, bodyFile string) (string, error) {
	if strings.TrimSpace(body) != "" && strings.TrimSpace(bodyFile) != "" {
		return "", usageErr(fmt.Errorf("use either --body or --body-file, not both"))
	}
	if strings.TrimSpace(bodyFile) != "" {
		data, err := os.ReadFile(bodyFile)
		if err != nil {
			return "", fmt.Errorf("reading --body-file: %w", err)
		}
		body = string(data)
	}
	if strings.TrimSpace(body) == "" {
		return "", usageErr(fmt.Errorf("either --body or --body-file is required"))
	}
	return body, nil
}

func inspectAttachments(paths []string) ([]draftAttachment, []draftPolicyIssue, error) {
	attachments := make([]draftAttachment, 0, len(paths))
	issues := make([]draftPolicyIssue, 0)
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		attachment := draftAttachment{
			Path:     path,
			Filename: filepath.Base(path),
			MIMEType: mime.TypeByExtension(strings.ToLower(filepath.Ext(path))),
		}
		if cause := blockedAttachmentReason(attachment.Filename); cause != "" {
			attachment.Blocked = true
			attachment.BlockCause = cause
			issues = append(issues, draftPolicyIssue{Severity: "error", Field: "attach", Message: fmt.Sprintf("blocked attachment %s: %s", attachment.Filename, cause)})
			attachments = append(attachments, attachment)
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, nil, fmt.Errorf("stat attachment %q: %w", path, err)
		}
		if info.IsDir() {
			return nil, nil, usageErr(fmt.Errorf("attachment %q is a directory", path))
		}
		attachment.Size = int(info.Size())
		attachments = append(attachments, attachment)
	}
	return attachments, issues, nil
}

func fieldIssuesForDraft(to, cc, bcc []string, subject, body string) []draftPolicyIssue {
	var issues []draftPolicyIssue
	if len(to) == 0 && len(cc) == 0 && len(bcc) == 0 {
		issues = append(issues, draftPolicyIssue{Severity: "warning", Field: "to", Message: "no recipients specified"})
	}
	if strings.TrimSpace(subject) == "" {
		issues = append(issues, draftPolicyIssue{Severity: "warning", Field: "subject", Message: "subject is empty"})
	}
	if strings.TrimSpace(body) == "" {
		issues = append(issues, draftPolicyIssue{Severity: "warning", Field: "body", Message: "body is empty"})
	}
	return issues
}

func hasBlockingIssue(issues []draftPolicyIssue) bool {
	for _, issue := range issues {
		if strings.EqualFold(issue.Severity, "error") {
			return true
		}
	}
	return false
}

func blockedAttachmentReason(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".pfx", ".pkcs12", ".p12":
		return "PKCS#12 certificate archives must not be attached by default"
	case ".key":
		return "private key files must not be attached by default"
	case ".pem", ".crt", ".cer", ".der":
		return "certificate or key material must be reviewed before sending"
	default:
		return ""
	}
}

func previewText(s string, limit int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

func buildDraftMIME(from string, to, cc, bcc []string, subject, body string, attachments []draftAttachment) ([]byte, error) {
	var buf bytes.Buffer
	headers := []string{}
	if from != "" {
		headers = append(headers, "From: "+from)
	}
	if len(to) > 0 {
		headers = append(headers, "To: "+strings.Join(to, ", "))
	}
	if len(cc) > 0 {
		headers = append(headers, "Cc: "+strings.Join(cc, ", "))
	}
	if len(bcc) > 0 {
		headers = append(headers, "Bcc: "+strings.Join(bcc, ", "))
	}
	headers = append(headers, "Subject: "+mime.QEncoding.Encode("utf-8", subject))
	headers = append(headers, "MIME-Version: 1.0")

	if len(attachments) == 0 {
		headers = append(headers, "Content-Type: text/plain; charset=UTF-8")
		headers = append(headers, "Content-Transfer-Encoding: quoted-printable")
		for _, h := range headers {
			_, _ = buf.WriteString(h + "\r\n")
		}
		_, _ = buf.WriteString("\r\n")
		qp := quotedprintable.NewWriter(&buf)
		if _, err := qp.Write([]byte(body)); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	mw := multipart.NewWriter(&buf)
	headers = append(headers, fmt.Sprintf("Content-Type: multipart/mixed; boundary=%q", mw.Boundary()))
	for _, h := range headers {
		_, _ = buf.WriteString(h + "\r\n")
	}
	_, _ = buf.WriteString("\r\n")

	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", "text/plain; charset=UTF-8")
	textHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	textPart, err := mw.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	qp := quotedprintable.NewWriter(textPart)
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}

	for _, attachment := range attachments {
		data, err := os.ReadFile(attachment.Path)
		if err != nil {
			return nil, fmt.Errorf("reading attachment %q: %w", attachment.Path, err)
		}
		mimeType := attachment.MIMEType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		partHeader := textproto.MIMEHeader{}
		partHeader.Set("Content-Type", fmt.Sprintf("%s; name=%q", mimeType, attachment.Filename))
		partHeader.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", attachment.Filename))
		partHeader.Set("Content-Transfer-Encoding", "base64")
		part, err := mw.CreatePart(partHeader)
		if err != nil {
			return nil, err
		}
		if err := writeBase64Lines(part, data); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeBase64Lines(w io.Writer, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	for len(encoded) > 76 {
		if _, err := io.WriteString(w, encoded[:76]+"\r\n"); err != nil {
			return err
		}
		encoded = encoded[76:]
	}
	if encoded != "" {
		if _, err := io.WriteString(w, encoded+"\r\n"); err != nil {
			return err
		}
	}
	return nil
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func confirmationHash(draftID, raw string) string {
	sum := sha256.Sum256([]byte(draftID + "\n" + raw))
	return hex.EncodeToString(sum[:8])
}

func fetchDraftRaw(ctx context.Context, c *client.Client, draftID string) (string, json.RawMessage, error) {
	path := "/gmail/v1/users/me/drafts/" + draftID
	data, err := c.Get(ctx, path, map[string]string{"format": "raw"})
	if err != nil {
		return "", nil, err
	}
	var resp struct {
		ID      string `json:"id"`
		Message struct {
			Raw string `json:"raw"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", nil, fmt.Errorf("parsing draft response: %w", err)
	}
	return resp.Message.Raw, data, nil
}
