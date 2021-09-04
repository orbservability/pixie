// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	rest "k8s.io/client-go/rest"
	testing "k8s.io/client-go/testing"
	v1alpha1 "px.dev/pixie/src/operator/client/versioned/typed/px.dev/v1alpha1"
)

type FakePxV1alpha1 struct {
	*testing.Fake
}

func (c *FakePxV1alpha1) Viziers(namespace string) v1alpha1.VizierInterface {
	return &FakeViziers{c, namespace}
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *FakePxV1alpha1) RESTClient() rest.Interface {
	var ret *rest.RESTClient
	return ret
}
