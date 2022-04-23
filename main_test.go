package main

import "testing"

func TestParseSource(t *testing.T) {
	testCases := []struct {
		gitHubLink string
		gitRef     string
	}{
		{gitHubLink: "https://github.com/quay/quay", gitRef: "HEAD"},
		{gitHubLink: "https://github.com/quay/quay/tree/master", gitRef: "refs/heads/master"},
		{gitHubLink: "https://github.com/quay/quay/tree/redhat-3.7", gitRef: "refs/heads/redhat-3.7"},
		{gitHubLink: "https://github.com/quay/quay/pull/1278", gitRef: "refs/pull/1278/head"},
		{gitHubLink: "https://github.com/quay/quay/releases/tag/v3.6.2", gitRef: "refs/tags/v3.6.2"},
	}
	for _, tc := range testCases {
		gitRef, err := ParseSource(tc.gitHubLink)
		if err != nil {
			t.Errorf("ParseSource(%q) failed: %v", tc.gitHubLink, err)
		}
		if gitRef != tc.gitRef {
			t.Errorf("ParseSource(%q): got %q, want %q", tc.gitHubLink, gitRef, tc.gitRef)
		}
	}
}

func TestGitRefSlug(t *testing.T) {
	testCases := []struct {
		gitRef string
		slug   string
	}{
		{gitRef: "refs/heads/master", slug: "master"},
		{gitRef: "refs/heads/redhat-3.7", slug: "redhat-3-7"},
		{gitRef: "refs/pull/1278/head", slug: "1278"},
		{gitRef: "refs/tags/v3.6.2", slug: "v3-6-2"},
	}
	for _, tc := range testCases {
		slug, err := GitRefSlug(tc.gitRef)
		if err != nil {
			t.Errorf("GitRefSlug(%q) failed: %v", tc.gitRef, err)
		}
		if slug != tc.slug {
			t.Errorf("GitRefSlug(%q): got %q, want %q", tc.gitRef, slug, tc.slug)
		}
	}
}
