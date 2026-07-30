package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

var tinyPNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

func TestPlaygroundOCRUploadSendsVisionContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil { t.Fatal(err) }
		if body["model"] != "vision-test" { t.Fatalf("unexpected OCR model %#v", body["model"]) }
		messages := body["messages"].([]interface{}); content := messages[0].(map[string]interface{})["content"].([]interface{})
		if content[1].(map[string]interface{})["type"] != "image_url" { t.Fatalf("vision content missing image_url: %#v", content) }
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"识别结果"}}],"usage":{"prompt_tokens":1}}`)
	}))
	defer upstream.Close()
	app := fiber.New(); setupPlaygroundEndpoints(app, &config.Config{OpenAIBaseURL:upstream.URL,OpenAIAPIKey:"chat-key",OCRModel:"vision-test"})
	baseURL := startLoopbackTestServer(t, app); token := playgroundToken(t, baseURL)
	var form bytes.Buffer; writer := multipart.NewWriter(&form); part, _ := writer.CreateFormFile("image", "fake.png"); _, _ = part.Write(tinyPNG); _ = writer.WriteField("prompt", "读取文字"); _ = writer.Close()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/playground/ocr/upload", &form); req.Header.Set("Content-Type", writer.FormDataContentType()); req.Header.Set("X-Playground-Token", token)
	resp, err := http.DefaultClient.Do(req); if err != nil { t.Fatal(err) }; data,_:=io.ReadAll(resp.Body);resp.Body.Close()
	if resp.StatusCode!=http.StatusOK || !strings.Contains(string(data),"识别结果") {t.Fatalf("unexpected OCR response %d %s",resp.StatusCode,data)}
}

func TestPlaygroundImageGenerationUsesDedicatedEndpointAndKey(t *testing.T) {
	encoded:=base64.StdEncoding.EncodeToString(tinyPNG)
	upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		if r.URL.Path!="/v1/images/generations" {t.Fatalf("unexpected image path %s",r.URL.Path)}
		if r.Header.Get("Authorization")!="Bearer image-key" {t.Fatalf("unexpected authorization %q",r.Header.Get("Authorization"))}
		_,_=io.WriteString(w,`{"data":[{"b64_json":"`+encoded+`","revised_prompt":"优化后"}]}`)
	}));defer upstream.Close()
	app:=fiber.New();setupPlaygroundEndpoints(app,&config.Config{ImageAPIURL:upstream.URL+"/v1",ImageAPIKey:"image-key",ImageModel:"image-model"});baseURL:=startLoopbackTestServer(t,app);token:=playgroundToken(t,baseURL)
	req,_:=http.NewRequest(http.MethodPost,baseURL+"/playground/images/generations",strings.NewReader(`{"prompt":"生成图片","size":"1024x1024","count":1}`));req.Header.Set("Content-Type","application/json");req.Header.Set("X-Playground-Token",token)
	resp,err:=http.DefaultClient.Do(req);if err!=nil{t.Fatal(err)};body,_:=io.ReadAll(resp.Body);resp.Body.Close();if resp.StatusCode!=http.StatusOK||!strings.Contains(string(body),encoded){t.Fatalf("unexpected image response %d %s",resp.StatusCode,body)}
}

func TestPlaygroundImageGenerationRequiresConfig(t *testing.T) {
	app:=fiber.New();setupPlaygroundEndpoints(app,&config.Config{});baseURL:=startLoopbackTestServer(t,app);token:=playgroundToken(t,baseURL)
	req,_:=http.NewRequest(http.MethodPost,baseURL+"/playground/images/generations",strings.NewReader(`{"prompt":"生成图片"}`));req.Header.Set("Content-Type","application/json");req.Header.Set("X-Playground-Token",token);resp,err:=http.DefaultClient.Do(req);if err!=nil{t.Fatal(err)};resp.Body.Close();if resp.StatusCode!=http.StatusServiceUnavailable{t.Fatalf("status=%d",resp.StatusCode)}
}

func playgroundToken(t *testing.T, baseURL string) string { t.Helper(); page:=getLoopbackTest(t,baseURL,"/playground");body,_:=io.ReadAll(page.Body);page.Body.Close();return extractLocalPageToken(t,string(body)) }
