package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"

	_ "github.com/distribution/distribution/v3/registry/auth/htpasswd"
	_ "github.com/distribution/distribution/v3/registry/auth/silly"
	_ "github.com/distribution/distribution/v3/registry/auth/token"
	_ "github.com/distribution/distribution/v3/registry/proxy"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/azure"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/gcs"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/inmemory"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/middleware/cloudfront"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/middleware/redirect"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/middleware/rewrite"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/s3-aws"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry"
	"github.com/sirupsen/logrus"
)

func getRegistrySock(ns string) string {
	return fmt.Sprintf("./var/%s/registry.sock", ns)
}

func setupProxy(ctx context.Context, namespace string, proxy configuration.Proxy) (*registry.Registry, error) {
	config := &configuration.Configuration{
		Version: "0.1",
		Storage: configuration.Storage{
			"filesystem": {
				"rootdirectory": fmt.Sprintf("./var/%s/registry", namespace),
			},
			"cache": {
				"blobdescriptor": "inmemory",
			},
		},
		Proxy: proxy,
	}
	config.Log.Level = "info"
	config.HTTP.Net = "unix"
	config.HTTP.Addr = getRegistrySock(namespace)

	os.MkdirAll(fmt.Sprintf("./var/%s", namespace), 0755)
	return registry.NewRegistry(ctx, config)
}

func startMainHttpServer(registries map[string]*registry.Registry) error {
	ln, err := net.Listen("tcp4", "0.0.0.0:5000")
	if err != nil {
		return err
	}
	defer ln.Close()

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logrus.Infof("[Proxy] %s %s", r.Method, r.RequestURI)
			namespace := "docker.io"
			if ns, ok := r.URL.Query()["ns"]; ok && len(ns) > 0 {
				if _, ok := registries[ns[0]]; ok {
					namespace = ns[0]
				}
			}
			logrus.Infof("[Proxy] namespace=%s", namespace)

			url := fmt.Sprintf("http://http.sock%s", r.RequestURI)
			unixReq, err := http.NewRequest(r.Method, url, r.Body)
			if err != nil {
				logrus.Infof("http request err: %s", err.Error())
				w.WriteHeader(500)
				return
			}
			unixReq.Header = r.Header
			client := &http.Client{
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
						return net.Dial("unix", getRegistrySock(namespace))
					},
				},
			}
			res, err := client.Do(unixReq)
			if err != nil {
				logrus.Infof("client.do err: %s", err.Error())
				w.WriteHeader(500)
				return
			}
			defer res.Body.Close()

			for k, vs := range res.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
					//logrus.Infof("Header %s=%v", k, v)
				}
			}
			w.WriteHeader(res.StatusCode)

			buf := make([]byte, 4096)
			for {
				n, err := res.Body.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
				}

				if err != nil {
					if !errors.Is(err, io.EOF) {
						logrus.Infof("read res body err: %s", err.Error())
					}
					break
				}
			}

		}),
	}
	return server.Serve(ln)
}

func setupRegistries(ctx context.Context, namespaces map[string]configuration.Proxy) (map[string]*registry.Registry, error) {
	registries := map[string]*registry.Registry{}
	for ns, url := range namespaces {
		r, err := setupProxy(ctx, ns, url)
		if err != nil {
			return nil, err
		}
		registries[ns] = r
	}

	return registries, nil
}

func main() {
	ctx := context.Background()
	os.Setenv("OTEL_TRACES_EXPORTER", "none")

	namespaces := map[string]configuration.Proxy{
		"docker.io": {
			RemoteURL: "https://dockerpull.org?ns=docker.io",
		},
		"quay.io": {
			RemoteURL: "https://dockerpull.org?ns=quay.io",
		},
		"registry.k8s.io": {
			RemoteURL: "https://dockerpull.org?ns=registry.k8s.io",
		},
		"cr.l5d.io": {
			RemoteURL: "https://dockerpull.org?ns=cr.l5d.io",
		},
		"container-registry.oracle.com": {
			RemoteURL: "https://container-registry.oracle.com",
		},
	}

	registries, err := setupRegistries(ctx, namespaces)
	if err != nil {
		logrus.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for _, r := range registries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.ListenAndServe(); err != nil {
				errCh <- err
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := startMainHttpServer(registries); err != nil {
			errCh <- err
		}
	}()

	go func() {
		wg.Wait()
		close(errCh)
	}()

	select {
	case err := <-errCh:
		logrus.Fatalln(err)
	case <-ctx.Done():
	}

}
