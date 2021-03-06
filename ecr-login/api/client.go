// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package api

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/awslabs/amazon-ecr-credential-helper/ecr-login/cache"
	"github.com/sirupsen/logrus"
)

const proxyEndpointScheme = "https://"
const programName = "docker-credential-ecr-login"

var ecrPattern = regexp.MustCompile(`(^[a-zA-Z0-9][a-zA-Z0-9-_]*)\.dkr\.ecr(\-fips)?\.([a-zA-Z0-9][a-zA-Z0-9-_]*)\.amazonaws\.com(\.cn)?`)

// Registry in ECR
type Registry struct {
	ID     string
	FIPS   bool
	Region string
}

// ExtractRegistry returns the ECR registry behind a given service endpoint
func ExtractRegistry(serverURL string) (*Registry, error) {
	if strings.HasPrefix(serverURL, proxyEndpointScheme) {
		serverURL = strings.TrimPrefix(serverURL, proxyEndpointScheme)
	}
	matches := ecrPattern.FindStringSubmatch(serverURL)
	if len(matches) == 0 {
		return nil, fmt.Errorf(programName + " can only be used with Amazon Elastic Container Registry.")
	} else if len(matches) < 3 {
		return nil, fmt.Errorf(serverURL + "is not a valid repository URI for Amazon Elastic Container Registry.")
	}
	registry := &Registry{
		ID:     matches[1],
		FIPS:   matches[2] == "-fips",
		Region: matches[3],
	}
	return registry, nil
}

// Client used for calling ECR service
type Client interface {
	GetCredentials(serverURL string) (*Auth, error)
	GetCredentialsByRegistryID(registryID string) (*Auth, error)
	ListCredentials() ([]*Auth, error)
}
type defaultClient struct {
	ecrClient       ECRAPI
	credentialCache cache.CredentialsCache
}
type ECRAPI interface {
	GetAuthorizationToken(*ecr.GetAuthorizationTokenInput) (*ecr.GetAuthorizationTokenOutput, error)
}

// Auth credentials returned by ECR service to allow docker login
type Auth struct {
	ProxyEndpoint string
	Username      string
	Password      string
}

// GetCredentials returns username, password, and proxyEndpoint
func (c *defaultClient) GetCredentials(serverURL string) (*Auth, error) {
	registry, err := ExtractRegistry(serverURL)
	if err != nil {
		return nil, err
	}
	logrus.
		WithField("registry", registry.ID).
		WithField("region", registry.Region).
		WithField("serverURL", serverURL).
		Debug("Retrieving credentials")
	return c.GetCredentialsByRegistryID(registry.ID)
}

// GetCredentials returns username, password, and proxyEndpoint
func (c *defaultClient) GetCredentialsByRegistryID(registryID string) (*Auth, error) {
	cachedEntry := c.credentialCache.Get(registryID)
	if cachedEntry != nil {
		if cachedEntry.IsValid(time.Now()) {
			logrus.WithField("registry", registryID).Debug("Using cached token")
			return extractToken(cachedEntry.AuthorizationToken, cachedEntry.ProxyEndpoint)
		}
		logrus.
			WithField("requestedAt", cachedEntry.RequestedAt).
			WithField("expiresAt", cachedEntry.ExpiresAt).
			Debug("Cached token is no longer valid")
	}

	auth, err := c.getAuthorizationToken(registryID)

	// if we have a cached token, fall back to avoid failing the request. This may result an expired token
	// being returned, but if there is a 500 or timeout from the service side, we'd like to attempt to re-use an
	// old token. We invalidate tokens prior to their expiration date to help mitigate this scenario.
	if err != nil && cachedEntry != nil {
		logrus.WithError(err).Info("Got error fetching authorization token. Falling back to cached token.")
		return extractToken(cachedEntry.AuthorizationToken, cachedEntry.ProxyEndpoint)
	}
	return auth, err
}

func (c *defaultClient) ListCredentials() ([]*Auth, error) {
	auths := []*Auth{}
	for _, authEntry := range c.credentialCache.List() {
		auth, err := extractToken(authEntry.AuthorizationToken, authEntry.ProxyEndpoint)
		if err != nil {
			logrus.WithError(err).Debug("Could not extract token")
		} else {
			auths = append(auths, auth)
		}
	}

	// If cache is empty, get authorization token of default registry
	if len(auths) == 0 {
		logrus.Debug("No credential cache")
		auth, err := c.getAuthorizationToken("")
		if err != nil {
			logrus.WithError(err).Debugf("Couldn't get authorization token")
		} else {
			auths = append(auths, auth)
		}
		return auths, err
	}

	return auths, nil
}

func (c *defaultClient) getAuthorizationToken(registryID string) (*Auth, error) {
	var input *ecr.GetAuthorizationTokenInput
	if registryID == "" {
		logrus.Debug("Calling ECR.GetAuthorizationToken for default registry")
		input = &ecr.GetAuthorizationTokenInput{}
	} else {
		logrus.WithField("registry", registryID).Debug("Calling ECR.GetAuthorizationToken")
		input = &ecr.GetAuthorizationTokenInput{
			RegistryIds: []*string{aws.String(registryID)},
		}
	}

	output, err := c.ecrClient.GetAuthorizationToken(input)
	if err != nil || output == nil {
		if err == nil {
			if registryID == "" {
				err = fmt.Errorf("missing AuthorizationData in ECR response for default registry")
			} else {
				err = fmt.Errorf("missing AuthorizationData in ECR response for %s", registryID)
			}
		}
		return nil, errors.Wrap(err, "ecr: Failed to get authorization token")
	}

	for _, authData := range output.AuthorizationData {
		if authData.ProxyEndpoint != nil && authData.AuthorizationToken != nil {
			authEntry := cache.AuthEntry{
				AuthorizationToken: aws.StringValue(authData.AuthorizationToken),
				RequestedAt:        time.Now(),
				ExpiresAt:          aws.TimeValue(authData.ExpiresAt),
				ProxyEndpoint:      aws.StringValue(authData.ProxyEndpoint),
			}
			registry, err := ExtractRegistry(authEntry.ProxyEndpoint)
			if err != nil {
				return nil, fmt.Errorf("Invalid ProxyEndpoint returned by ECR: %s", authEntry.ProxyEndpoint)
			}
			auth, err := extractToken(authEntry.AuthorizationToken, authEntry.ProxyEndpoint)
			if err != nil {
				return nil, err
			}
			c.credentialCache.Set(registry.ID, &authEntry)
			return auth, nil
		}
	}
	if registryID == "" {
		return nil, fmt.Errorf("No AuthorizationToken found for default registry")
	}
	return nil, fmt.Errorf("No AuthorizationToken found for %s", registryID)
}

func extractToken(token string, proxyEndpoint string) (*Auth, error) {
	decodedToken, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("Invalid token: %v", err)
	}

	parts := strings.SplitN(string(decodedToken), ":", 2)
	if len(parts) < 2 {
		return nil, fmt.Errorf("Invalid token: expected two parts, got %d", len(parts))
	}

	return &Auth{
		Username:      parts[0],
		Password:      parts[1],
		ProxyEndpoint: proxyEndpoint,
	}, nil
}
