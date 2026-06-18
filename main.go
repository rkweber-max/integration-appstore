package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
)

var (
	clientID     string
	clientSecret string
	appID        string
	externalKey  string
	callbackURL  string
	storeBaseURL string
	apiBaseURL   string
	authURL      string
	port         string
)

func main() {
	loadDotEnv(".env")

	clientID = mustEnv("APP_CLIENT_ID")
	clientSecret = mustEnv("APP_CLIENT_SECRET")
	appID = mustEnv("APP_ID")
	callbackURL = mustEnv("CALLBACK_URL")
	storeBaseURL = mustEnv("STORE_INTEGRATION_URL")
	externalKey = getenv("APP_EXTERNAL_KEY", "teste-local-001")
	apiBaseURL = getenv("API_BASE_URL", "https://api.sandboxappmax.com.br")
	authURL = getenv("AUTH_URL", "https://auth.sandboxappmax.com.br/oauth2/token")
	port = getenv("PORT", "8080")

	http.HandleFunc("/", installApp)
	http.HandleFunc("/callback/install", callbackInstall)
	http.HandleFunc("/finish", installFinished)

	fmt.Printf("Servidor iniciado na porta %s\n", port)

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		panic(err)
	}
}

func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)

		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("variável de ambiente obrigatória ausente: %s", key)
	}
	return v
}

func installApp(w http.ResponseWriter, r *http.Request) {

	tokenResponse, _, err := getToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	accessToken, ok := tokenResponse["access_token"].(string)
	if !ok {
		http.Error(w, "access_token não encontrado", http.StatusInternalServerError)
		return
	}

	authResponse, _, err := appAuthorize(accessToken)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data, ok := authResponse["data"].(map[string]interface{})
	if !ok {
		http.Error(w, "campo data inválido", http.StatusInternalServerError)
		return
	}

	installationToken, ok := data["token"].(string)
	if !ok {
		http.Error(w, "token de instalação inválido", http.StatusInternalServerError)
		return
	}

	redirectURL := storeBaseURL + installationToken

	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func installFinished(w http.ResponseWriter, r *http.Request) {

	token := r.URL.Query().Get("token")

	fmt.Printf("[installFinished] %s token=%q\n", r.Method, token)

	if token == "" {
		http.Error(w, "token não informado", http.StatusBadRequest)
		return
	}

	appCredentials, _, err := getToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	accessToken, ok := appCredentials["access_token"].(string)
	if !ok {
		http.Error(w, "access_token não encontrado", http.StatusInternalServerError)
		return
	}

	result, status, err := createMerchantCredentials(accessToken, token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if status >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Falha ao gerar credenciais do merchant",
			"status":  status,
			"data":    result,
		})
		return
	}

	merchantID, merchantSecret, err := extractMerchantClient(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	merchantToken, tokenStatus, err := requestToken(merchantID, merchantSecret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if tokenStatus >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(tokenStatus)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Falha ao obter token do merchant",
			"status":  tokenStatus,
			"data":    merchantToken,
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")

	response := map[string]interface{}{
		"message":        "Instalação concluída com sucesso",
		"data":           result,
		"merchant_token": merchantToken,
	}

	json.NewEncoder(w).Encode(response)
}

func callbackInstall(w http.ResponseWriter, r *http.Request) {

	if token := r.URL.Query().Get("token"); token != "" {
		installFinished(w, r)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := map[string]string{
		"external_id": uuid.New().String(),
	}

	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(response)
}

func createMerchantCredentials(bearerToken string, token string) (map[string]interface{}, int, error) {

	payload := map[string]interface{}{
		"token": token,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest(
		"POST",
		apiBaseURL+"/app/client/generate",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	fmt.Printf("[createMerchantCredentials] POST %s payload=%s\n", req.URL, string(jsonData))

	client := &http.Client{}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	fmt.Printf("[createMerchantCredentials] status=%d body=%s\n", resp.StatusCode, string(body))

	var result map[string]interface{}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("resposta não-JSON (status %d): %s", resp.StatusCode, string(body))
	}

	return result, resp.StatusCode, nil
}

func appAuthorize(bearerToken string) (map[string]interface{}, int, error) {

	payload := map[string]interface{}{
		"app_id":       appID,
		"external_key": externalKey,
		"url_callback": callbackURL,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequest(
		"POST",
		apiBaseURL+"/app/authorize",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	client := &http.Client{}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	var result map[string]interface{}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, resp.StatusCode, err
	}

	return result, resp.StatusCode, nil
}

func getToken() (map[string]interface{}, int, error) {
	return requestToken(clientID, clientSecret)
}

func requestToken(id, secret string) (map[string]interface{}, int, error) {

	formData := url.Values{}
	formData.Set("grant_type", "client_credentials")
	formData.Set("client_id", id)
	formData.Set("client_secret", secret)

	req, err := http.NewRequest(
		"POST",
		authURL,
		bytes.NewBufferString(formData.Encode()),
	)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	fmt.Printf("[requestToken] client_id=%s status=%d body=%s\n", id, resp.StatusCode, string(body))

	var result map[string]interface{}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("resposta não-JSON (status %d): %s", resp.StatusCode, string(body))
	}

	return result, resp.StatusCode, nil
}

func extractMerchantClient(result map[string]interface{}) (string, string, error) {
	data, ok := result["data"].(map[string]interface{})
	if !ok {
		return "", "", fmt.Errorf("campo 'data' ausente na resposta de credenciais")
	}

	client, ok := data["client"].(map[string]interface{})
	if !ok {
		return "", "", fmt.Errorf("campo 'data.client' ausente na resposta de credenciais")
	}

	id, ok := client["client_id"].(string)
	if !ok {
		return "", "", fmt.Errorf("client_id ausente na resposta de credenciais")
	}

	secret, ok := client["client_secret"].(string)
	if !ok {
		return "", "", fmt.Errorf("client_secret ausente na resposta de credenciais")
	}

	return id, secret, nil
}
