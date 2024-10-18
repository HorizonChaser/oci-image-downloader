package main

//DONE marshall then unmarshall may cause SHA256 mismatch in manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Layer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}

type ManifestV2 struct {
	SchemaVersion int     `json:"schemaVersion"`
	MediaType     string  `json:"mediaType"`
	Config        Layer   `json:"config"`
	Layers        []Layer `json:"layers"`
}

type ManifestV1 struct {
	FsLayers []struct {
		BlobSum string `json:"blobSum"`
	} `json:"fsLayers"`
	History []struct {
		V1Compatibility string `json:"v1Compatibility"`
	} `json:"history"`
}

type ManifestList struct {
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			Variant      string `json:"variant,omitempty"`
		} `json:"platform"`
		Size int `json:"size"`
	} `json:"manifests"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("usage: %s <output-dir> <image[:tag][@digest]> ...", os.Args[0])
		os.Exit(1)
	}
	dir := os.Args[1]
	images := os.Args[2:]

	if err := os.MkdirAll(dir, 0755); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: failed to create directory: %v\n", err)
		os.Exit(1)
	}

	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "error: failed to create directory: %v\n", err)
		os.Exit(1)
	}

	err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte("{\"imageLayoutVersion\": \"1.0.0\"}"), 0644)
	if err != nil {
		panic(err)
	}

	for _, imageTag := range images {
		if err := processImage(dir, imageTag); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "error: failed to process image %s: %v\n", imageTag, err)
			os.Exit(1)
		}
	}

	fmt.Printf("Download of images into '%s' complete.\n", dir)
	fmt.Println("Use something like the following to load the result into a containerd instance:")
	fmt.Printf("  tar -cC '%s' . | nerdctl load\n", dir)
}

func processImage(dir, imageTag string) error {
	// parse image tag, use latest as default
	image := strings.Split(imageTag, ":")[0]
	tag := "latest"
	if strings.Contains(imageTag, ":") {
		tag = strings.Split(imageTag, ":")[1]
	}

	// add prefix library if official image has been passed
	if !strings.Contains(image, "/") {
		image = "library/" + image
	}

	token, err := fetchAuthToken(image)
	if err != nil {
		return err
	}

	manifestJson, err := fetchManifest(token, image, tag)
	if err != nil {
		return err
	}

	var manifest map[string]interface{}
	if err := manifestJson.Decode(&manifest); err != nil {
		return err
	}

	//manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	//if err != nil {
	//	return err
	//}

	//fmt.Println("Manifest JSON string in processImage:")
	//fmt.Println(string(manifestBytes))

	schemaVersion := int(manifest["schemaVersion"].(float64))
	if schemaVersion == 1 {
		if err := handleManifestV1(manifest, token, image, dir); err != nil {
			return err
		}
	} else if schemaVersion == 2 {
		mediaType := manifest["mediaType"].(string)
		switch mediaType {
		//for nginx
		case "application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.oci.image.index.v1+json":
			if err := handleManifestList(manifest, token, image, dir); err != nil {
				return err
			}
		case "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.oci.image.manifest.v1+json":
			if err := handleManifestV2(manifest, token, image, dir); err != nil {
				return err
			}
		default:
			return errors.New("unsupported manifest media type")
		}
	} else {
		return errors.New("unknown schema version")
	}

	return nil
}

func fetchAuthToken(image string) (string, error) {
	pullUrl := fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", image)
	resp, err := httpGet(pullUrl)
	if err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			panic(err)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", errors.New("failed to fetch auth token")
	}

	var data struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}

	return data.Token, nil
}

func fetchManifest(token, image, tag string) (*json.Decoder, error) {
	manifestUrl := fmt.Sprintf("https://registry-1.docker.io/v2/%s/manifests/%s", image, tag)
	req, err := http.NewRequest("GET", manifestUrl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v1+json")

	resp, err := httpDo(req)
	if err != nil {
		return nil, err
	}
	//defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("failed to fetch manifest")
	}

	return json.NewDecoder(resp.Body), nil
}

func fetchManifestRaw(token, image, tag string) (io.ReadCloser, error) {
	manifestUrl := fmt.Sprintf("https://registry-1.docker.io/v2/%s/manifests/%s", image, tag)
	req, err := http.NewRequest("GET", manifestUrl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v1+json")

	resp, err := httpDo(req)
	if err != nil {
		return nil, err
	}
	//defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("failed to fetch manifest")
	}

	return resp.Body, nil
}

func handleManifestV1(manifest map[string]interface{}, token, image, dir string) error {
	var manifestV1 ManifestV1
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(manifestBytes, &manifestV1); err != nil {
		return err
	}

	fmt.Printf("Downloading '%s' (%d layers)...\n", image, len(manifestV1.FsLayers))
	for i, layer := range manifestV1.FsLayers {
		layerPath := filepath.Join(dir, fmt.Sprintf("layer-%d.tar.gz", i))
		if err := downloadLayer(token, image, layer.BlobSum, layerPath); err != nil {
			return err
		}
	}

	return nil
}

func handleManifestV2(manifest map[string]interface{}, token, image, dir string) error {
	var manifestV2 ManifestV2
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(manifestBytes, &manifestV2); err != nil {
		return err
	}

	//fmt.Println("ManifestV2 in handleManifestV2:")
	//fmt.Println(string(manifestBytes))

	fmt.Printf("Downloading '%s' (%d layers)...\n", image, len(manifestV2.Layers))
	for _, layer := range manifestV2.Layers {
		layerPath := filepath.Join(filepath.Join(dir, "blobs", "sha256"), strings.ReplaceAll(layer.Digest, "sha256:", ""))
		if err := downloadLayer(token, image, layer.Digest, layerPath); err != nil {
			return err
		}
	}

	return nil
}

func handleManifestList(manifest map[string]interface{}, token, image, dir string) error {
	var manifestList ManifestList
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(manifestBytes, &manifestList); err != nil {
		return err
	}

	//fmt.Printf("ManifestList in handleManifestList: %s\n", string(manifestBytes))

	targetArch := os.Getenv("TARGETARCH")
	if targetArch == "" {
		targetArch = "amd64"
	}
	for _, manifestRef := range manifestList.Manifests {
		if manifestRef.Platform.Architecture == targetArch {
			toPrint := fmt.Sprintf("{\"schemaVersion\":2,\"manifests\":[{\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"digest\":\"%s\",\"size\":%d}]}", manifestRef.Digest, manifestRef.Size)

			err = os.WriteFile(filepath.Join(dir, "index.json"), []byte(toPrint), 0644)
			if err != nil {
				panic(err)
			}

			return handleManifestByDigest(token, image, manifestRef.Digest, dir)
		}
	}
	return errors.New("no matching manifest for target architecture")
}

func handleManifestByDigest(token, image, digest, dir string) error {
	manifestJson, err := fetchManifest(token, image, digest)
	if err != nil {
		return err
	}

	var manifest map[string]interface{}
	if err := manifestJson.Decode(&manifest); err != nil {
		return err
	}

	//manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	//if err != nil {
	//	return err
	//}
	//fmt.Println("Manifest JSON string in handleManifestByDigest:")
	//fmt.Println(string(manifestBytes))

	//write manifest json to blobs
	rawManifest, err := fetchManifestRaw(token, image, digest)
	rawManifestBytes, err := io.ReadAll(rawManifest)
	blobDir := filepath.Join(dir, "blobs", "sha256")
	err = os.WriteFile(filepath.Join(blobDir, strings.ReplaceAll(digest, "sha256:", "")), rawManifestBytes, 0644)
	if err != nil {
		panic(err)
	}

	// 获取 config.digest 的值
	config, ok := manifest["config"].(map[string]interface{})
	if !ok {
		return errors.New("invalid config format")
	}
	configDigest, ok := config["digest"].(string)
	if !ok {
		return errors.New("invalid digest format")
	}
	fmt.Println("Config digest:", configDigest)
	err = downloadConfig(token, image, configDigest, blobDir)

	mediaType := manifest["mediaType"].(string)
	switch mediaType {
	case "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.oci.image.manifest.v1+json":
		return handleManifestV2(manifest, token, image, dir)
	default:
		return errors.New("unsupported manifest media type for digest")
	}
}

func downloadLayer(token, image, digest, layerPath string) error {
	layerUrl := fmt.Sprintf("https://registry-1.docker.io/v2/%s/blobs/%s", image, digest)
	req, err := http.NewRequest("GET", layerUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpDo(req)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			panic(err)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return errors.New("failed to download layer")
	}

	file, err := os.Create(layerPath)
	if err != nil {
		return err
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			panic(err)
		}
	}(file)

	if _, err := io.Copy(file, resp.Body); err != nil {
		return err
	}

	return nil
}

func downloadConfig(token, image, digest, blobDir string) error {
	configUrl := fmt.Sprintf("https://registry-1.docker.io/v2/%s/blobs/%s", image, digest)
	req, err := http.NewRequest("GET", configUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpDo(req)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			panic(err)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return errors.New("failed to download config")
	}

	file, err := os.Create(path.Join(blobDir, strings.ReplaceAll(digest, "sha256:", "")))
	if err != nil {
		return err
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			panic(err)
		}
	}(file)

	if _, err := io.Copy(file, resp.Body); err != nil {
		return err
	}

	return nil
}

func httpGet(urlIn string) (*http.Response, error) {
	client := &http.Client{}
	if proxyURL := os.Getenv("HTTP_PROXY"); proxyURL != "" {
		proxy, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxy)}
	}
	return client.Get(urlIn)
}

func httpDo(req *http.Request) (*http.Response, error) {

	//fmt.Println("httpDo: req.URL: ", req.URL)

	client := &http.Client{}
	if proxyURL := os.Getenv("HTTP_PROXY"); proxyURL != "" {
		proxy, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(proxy)}
	}
	return client.Do(req)
}
