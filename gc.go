// reggc is a service to clean up unused container images.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/ref"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type stringset = map[string]struct{}

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	ticker := time.NewTicker(time.Hour)
	for {
		if err := run(ctx); err != nil && ctx.Err() == nil {
			slog.Error(err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

var (
	rc   *regclient.RegClient
	k8s  *kubernetes.Clientset
	rcfg *rest.Config
)

func run(ctx context.Context) error {
	rc = regclient.New(
		regclient.WithConfigHost(
			config.Host{Name: "registry:5000", TLS: config.TLSDisabled},
		),
	)
	var err error
	rcfg, err = rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("could not get cluster config: %w", err)
	}
	k8s, err = kubernetes.NewForConfig(rcfg)
	if err != nil {
		return fmt.Errorf("could not make kubernetes client: %w", err)
	}

	regimgs, err := fetchRegistryImages(ctx)
	if err != nil {
		return fmt.Errorf("could not fetch registry images: %w", err)
	}
	podimgs, err := fetchPodImages(ctx)
	if err != nil {
		return fmt.Errorf("could not fetch pod images: %w", err)
	}
	for img := range regimgs {
		if _, ok := podimgs[img]; ok {
			delete(regimgs, img)
		}
	}
	for img := range regimgs {
		if err := deleteImage(ctx, img); err != nil {
			return fmt.Errorf("could not delete image %q: %w", img, err)
		}
	}
	if err := gcRegistry(ctx); err != nil {
		return fmt.Errorf("could not trigger registry gc: %w", err)
	}
	return nil
}

func fetchRegistryImages(ctx context.Context) (stringset, error) {
	// TODO: Also ignore recent uploads here, if possible.
	// Manifests may have timestamps and could be used for a grace period.
	imgs := make(stringset)
	repos, err := rc.RepoList(ctx, "registry:5000")
	if err != nil {
		return nil, fmt.Errorf("could not get repository list: %w", err)
	}
	for _, repo := range repos.RepoRegistryList.Repositories {
		tags, err := rc.TagList(ctx, ref.Ref{
			Scheme:     "reg",
			Repository: repo,
			Registry:   "registry:5000",
		})
		if err != nil {
			return nil, fmt.Errorf("could not get tags for repository %q: %w",
				repo, err,
			)
		}
		for _, tag := range tags.Tags {
			imgs["ctr.lesiw.dev/"+repo+":"+tag] = struct{}{}
		}
	}
	return imgs, nil
}

func fetchPodImages(ctx context.Context) (stringset, error) {
	imgs := make(stringset)
	pods, err := k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		for _, ctr := range pod.Spec.Containers {
			imgs[ctr.Image] = struct{}{}
		}
	}
	return imgs, nil
}

func deleteImage(ctx context.Context, img string) error {
	_, repotag, ok := strings.Cut(img, "/")
	if !ok {
		return fmt.Errorf("could not parse registry from %q", img)
	}
	repo, tag, ok := strings.Cut(repotag, ":")
	if !ok {
		return fmt.Errorf("could not parse repo and tag from %q", repotag)
	}
	err := rc.TagDelete(ctx, ref.Ref{
		Registry:   "registry:5000",
		Repository: repo,
		Tag:        tag,
		Scheme:     "reg",
	})
	if err != nil {
		return fmt.Errorf("could not delete %q: %w", img, err)
	}
	slog.Info("deleted image", "image", img)
	return nil
}

func gcRegistry(ctx context.Context) error {
	req := k8s.CoreV1().RESTClient().Post().
		Resource("pods").
		Name("registry-0").
		Namespace("default").
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Command: []string{
			"bin/registry", "garbage-collect", "--delete-untagged",
			"/etc/docker/registry/config.yml",
		},
		Stdout: true,
		Stderr: true,
	}, scheme.ParameterCodec)

	spdyExec, err := remotecommand.NewSPDYExecutor(rcfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("could not create spdy executor: %w", err)
	}
	wsExec, err := remotecommand.NewWebSocketExecutor(
		rcfg, "GET", req.URL().String(),
	)
	if err != nil {
		return fmt.Errorf("failed to create websocket executor: %w", err)
	}
	exec, _ := remotecommand.NewFallbackExecutor(wsExec, spdyExec,
		func(err error) bool {
			return httpstream.IsUpgradeFailure(err) ||
				httpstream.IsHTTPSProxyError(err)
		},
	)
	var buf bytes.Buffer
	err = exec.StreamWithContext(ctx,
		remotecommand.StreamOptions{
			Stdout: &buf,
			Stderr: &buf,
		},
	)
	if err != nil && buf.Len() > 0 {
		return fmt.Errorf(
			"could not exec registry garbage-collect: %w\n---\n%s",
			err, buf.String(),
		)
	} else if err != nil {
		return fmt.Errorf("could not exec registry garbage-collect: %w", err)
	}
	slog.Info("gc completed")
	return nil
}
