package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	} `json:"manifests"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: go run main.go <output-dir> <image[:tag][@digest]> ...")
		os.Exit(1)
	}
	dir := os.Args[1]
	images := os.Args[2:]

	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to create directory: %v\n", err)
		os.Exit(1)
	}

	for _, imageTag := range images {
		if err := processImage(dir, imageTag); err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to process image %s: %v\n", imageTag, err)
			os.Exit(1)
		}
	}

	fmt.Printf("Download of images into '%s' complete.\n", dir)
	fmt.Println("Use something like the following to load the result into a Docker daemon:")
	fmt.Printf("  tar -cC '%s' . | docker load\n", dir)
}

func processImage(dir, imageTag string) error {
	image := strings.Split(imageTag, ":")[0]
	tag := "latest"
	if strings.Contains(imageTag, ":") {
		tag = strings.Split(imageTag, ":")[1]
	}

	// add prefix library if passed official image
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

	schemaVersion := int(manifest["schemaVersion"].(float64))
	if schemaVersion == 1 {
		if err := handleManifestV1(manifest, token, image, dir); err != nil {
			return err
		}
	} else if schemaVersion == 2 {
		mediaType := manifest["mediaType"].(string)
		switch mediaType {
		case "application/vnd.docker.distribution.manifest.list.v2+json":
			if err := handleManifestList(manifest, token, image, dir); err != nil {
				return err
			}
		case "application/vnd.docker.distribution.manifest.v2+json":
			if err := handleManifestV2(manifest, token, image, dir); err != nil {
				return err
			}
		case "application/vnd.oci.image.index.v1+json":
			if err := handleManifestList(manifest, token, image, dir); err != nil {
				return err
			}
		case "application/vnd.oci.image.manifest.v1+json":
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
	url := fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", image)
	resp, err := httpGet(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

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
	url := fmt.Sprintf("https://registry-1.docker.io/v2/%s/manifests/%s", image, tag)
	req, err := http.NewRequest("GET", url, nil)
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

	fmt.Printf("Downloading '%s' (%d layers)...\n", image, len(manifestV2.Layers))
	for _, layer := range manifestV2.Layers {
		layerPath := filepath.Join(dir, strings.ReplaceAll(layer.Digest, ":", "_")) + ".tar.gz"
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

	targetArch := os.Getenv("TARGETARCH")
	if targetArch == "" {
		targetArch = "amd64"
	}
	for _, manifestRef := range manifestList.Manifests {
		if manifestRef.Platform.Architecture == targetArch {
			return processImage(dir, fmt.Sprintf("%s@%s", image, manifestRef.Digest))
		}
	}
	return errors.New("no matching manifest for target architecture")
}

func downloadLayer(token, image, digest, layerPath string) error {
	url := fmt.Sprintf("https://registry-1.docker.io/v2/%s/blobs/%s", image, digest)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New("failed to download layer")
	}

	file, err := os.Create(layerPath)
	if err != nil {
		return err
	}
	defer file.Close()

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
