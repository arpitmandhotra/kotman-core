package meta

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "net/http"
    "net/url"
    "os"
    "time"
)

type AudienceClient struct {
    httpClient      *http.Client
    graphAPIVersion string
    graphAPIBase    string
}

type AudienceUploadResult struct {
    AudienceID        string
    NumUsersAdded     int
    NumUsersRejected  int
}

func NewAudienceClient() *AudienceClient {
    apiBase := os.Getenv("META_GRAPH_API_BASE")
    if apiBase == "" {
        apiBase = "https://graph.facebook.com"
    }
    return &AudienceClient{
        httpClient: &http.Client{
            Timeout: 30 * time.Second,
        },
        graphAPIVersion: "v21.0",
        graphAPIBase:    apiBase,
    }
}

func (c *AudienceClient) UploadVerifiedBuyers(ctx context.Context,
    adAccountID string,
    accessToken string,
    audienceName string,
    phoneHashesMeta []string) (AudienceUploadResult, error) {

    // 1. GUARD: minimum 50 required for lookalike
    if len(phoneHashesMeta) < 50 {
        return AudienceUploadResult{}, fmt.Errorf("audience too small: %d buyers, minimum 50 required for Meta lookalike to be meaningful", len(phoneHashesMeta))
    }

    // 2. FIND OR CREATE the custom audience
    queryURL := fmt.Sprintf("%s/%s/%s/customaudiences?fields=id,name&access_token=%s",
        c.graphAPIBase, c.graphAPIVersion, adAccountID, url.QueryEscape(accessToken))

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, nil)
    if err != nil {
        return AudienceUploadResult{}, fmt.Errorf("failed to build list audiences request: %w", err)
    }

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return AudienceUploadResult{}, fmt.Errorf("failed to call list audiences: %w", err)
    }
    defer func() {
        io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
        resp.Body.Close()
    }()

    if resp.StatusCode >= 400 {
        respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
        return AudienceUploadResult{}, fmt.Errorf("list audiences failed (status %d): %s", resp.StatusCode, string(respBytes))
    }

    var listResp struct {
        Data []struct {
            ID   string `json:"id"`
            Name string `json:"name"`
        } `json:"data"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
        return AudienceUploadResult{}, fmt.Errorf("failed to decode list audiences response: %w", err)
    }

    audienceID := ""
    for _, aud := range listResp.Data {
        if aud.Name == audienceName {
            audienceID = aud.ID
            break
        }
    }

    if audienceID == "" {
        // Create custom audience
        createURL := fmt.Sprintf("%s/%s/%s/customaudiences", c.graphAPIBase, c.graphAPIVersion, adAccountID)
        createPayload := map[string]interface{}{
            "name":                 audienceName,
            "subtype":              "CUSTOM",
            "description":          "Kaughtman-verified high-trust COD buyers",
            "customer_file_source": "PARTNER_PROVIDED_ONLY",
            "access_token":         accessToken,
        }
        bodyBytes, err := json.Marshal(createPayload)
        if err != nil {
            return AudienceUploadResult{}, fmt.Errorf("failed to marshal create audience payload: %w", err)
        }

        req, err = http.NewRequestWithContext(ctx, http.MethodPost, createURL, bytes.NewReader(bodyBytes))
        if err != nil {
            return AudienceUploadResult{}, fmt.Errorf("failed to build create audience request: %w", err)
        }
        req.Header.Set("Content-Type", "application/json")

        createResp, err := c.httpClient.Do(req)
        if err != nil {
            return AudienceUploadResult{}, fmt.Errorf("failed to call create audience: %w", err)
        }
        defer func() {
            io.Copy(io.Discard, io.LimitReader(createResp.Body, 1<<20))
            createResp.Body.Close()
        }()

        if createResp.StatusCode >= 400 {
            respBytes, _ := io.ReadAll(io.LimitReader(createResp.Body, 4096))
            return AudienceUploadResult{}, fmt.Errorf("create audience failed (status %d): %s", createResp.StatusCode, string(respBytes))
        }

        var createResult struct {
            ID string `json:"id"`
        }
        if err := json.NewDecoder(createResp.Body).Decode(&createResult); err != nil {
            return AudienceUploadResult{}, fmt.Errorf("failed to decode create audience response: %w", err)
        }
        audienceID = createResult.ID
        slog.Info("meta_audience: created new custom audience", "audience_id", audienceID, "name", audienceName)
    } else {
        slog.Info("meta_audience: using existing custom audience", "audience_id", audienceID, "name", audienceName)
    }

    // 3. CHUNK the phoneHashesMeta slice into batches of 10,000
    totalAdded := 0
    totalRejected := 0
    batchSize := 10000

    for i := 0; i < len(phoneHashesMeta); i += batchSize {
        end := i + batchSize
        if end > len(phoneHashesMeta) {
            end = len(phoneHashesMeta)
        }
        batch := phoneHashesMeta[i:end]

        // Format data: [[hash1], [hash2], ...]
        dataPayload := make([][]string, len(batch))
        for j, hash := range batch {
            dataPayload[j] = []string{hash}
        }

        uploadPayload := map[string]interface{}{
            "schema":       []string{"PHONE"},
            "data":         dataPayload,
            "access_token": accessToken,
        }

        uploadURL := fmt.Sprintf("%s/%s/%s/users", c.graphAPIBase, c.graphAPIVersion, audienceID)
        bodyBytes, err := json.Marshal(uploadPayload)
        if err != nil {
            return AudienceUploadResult{}, fmt.Errorf("failed to marshal upload payload: %w", err)
        }

        req, err = http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(bodyBytes))
        if err != nil {
            return AudienceUploadResult{}, fmt.Errorf("failed to build upload request: %w", err)
        }
        req.Header.Set("Content-Type", "application/json")

        uploadResp, err := c.httpClient.Do(req)
        if err != nil {
            return AudienceUploadResult{}, fmt.Errorf("failed to call upload users: %w", err)
        }
        defer func() {
            io.Copy(io.Discard, io.LimitReader(uploadResp.Body, 1<<20))
            uploadResp.Body.Close()
        }()

        if uploadResp.StatusCode >= 400 {
            respBytes, _ := io.ReadAll(io.LimitReader(uploadResp.Body, 4096))
            return AudienceUploadResult{}, fmt.Errorf("upload users failed (status %d): %s", uploadResp.StatusCode, string(respBytes))
        }

        var uploadResult struct {
            NumReceived int `json:"num_received"`
            NumInvalid  int `json:"num_invalid"`
        }
        if err := json.NewDecoder(uploadResp.Body).Decode(&uploadResult); err != nil {
            return AudienceUploadResult{}, fmt.Errorf("failed to decode upload users response: %w", err)
        }

        totalAdded += uploadResult.NumReceived
        totalRejected += uploadResult.NumInvalid
    }

    return AudienceUploadResult{
        AudienceID:       audienceID,
        NumUsersAdded:    totalAdded,
        NumUsersRejected: totalRejected,
    }, nil
}
