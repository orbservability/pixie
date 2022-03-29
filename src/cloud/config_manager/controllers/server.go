/*
 * Copyright 2018- The Pixie Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package controllers

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gofrs/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"google.golang.org/grpc/metadata"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"

	atpb "px.dev/pixie/src/cloud/artifact_tracker/artifacttrackerpb"
	cpb "px.dev/pixie/src/cloud/config_manager/configmanagerpb"
	"px.dev/pixie/src/cloud/vzmgr/vzmgrpb"
	versionspb "px.dev/pixie/src/shared/artifacts/versionspb"
	srvutils "px.dev/pixie/src/shared/services/utils"
	"px.dev/pixie/src/utils"
	"px.dev/pixie/src/utils/shared/tar"
	yamls "px.dev/pixie/src/utils/shared/yamls"
	vizieryamls "px.dev/pixie/src/utils/template_generator/vizier_yamls"
)

// Server defines an gRPC server type.
type Server struct {
	atClient            atpb.ArtifactTrackerClient
	deployKeyClient     vzmgrpb.VZDeploymentKeyServiceClient
	vzFeatureFlagClient VizierFeatureFlagClient
	clientset           *kubernetes.Clientset
	rm                  meta.RESTMapper
}

// NewServer creates GRPC handlers.
func NewServer(atClient atpb.ArtifactTrackerClient, deployKeyClient vzmgrpb.VZDeploymentKeyServiceClient, ldSDKKey string, clientset *kubernetes.Clientset, rm meta.RESTMapper) *Server {
	return &Server{
		atClient:            atClient,
		deployKeyClient:     deployKeyClient,
		vzFeatureFlagClient: NewVizierFeatureFlagClient(ldSDKKey),
		clientset:           clientset,
		rm:                  rm,
	}
}

func downloadFile(url string) (io.ReadCloser, error) {
	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func getServiceCredentials(signingKey string) (string, error) {
	claims := srvutils.GenerateJWTForService("ConfigManager Service", viper.GetString("domain_name"))
	return srvutils.SignJWTClaims(claims, signingKey)
}

// Helper function that looks up the org ID based on the deploy key so we can use it to set feature flags.
func (s *Server) getOrgIDForDeployKey(deployKey string) (uuid.UUID, error) {
	serviceAuthToken, err := getServiceCredentials(viper.GetString("jwt_signing_key"))
	if err != nil {
		return uuid.Nil, err
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization",
		fmt.Sprintf("bearer %s", serviceAuthToken))
	resp, err := s.deployKeyClient.LookupDeploymentKey(ctx, &vzmgrpb.LookupDeploymentKeyRequest{
		Key: deployKey,
	})
	if err != nil || resp == nil || resp.Key == nil {
		return uuid.Nil, fmt.Errorf("Error fetching deployment key org ID: %s", err.Error())
	}
	orgID, err := utils.UUIDFromProto(resp.Key.OrgID)
	if err != nil || orgID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("Error parsing org ID as proto: %s", err.Error())
	}
	return orgID, err
}

const (
	bytesPerMiB                     = 1024 * 1024
	defaultTableStorePercentage     = 0.6
	defaultUncappedTableStoreSizeMB = 1228
	tableStoreSizePEMFlag           = "PL_TABLE_STORE_DATA_LIMIT_MB"
)

// AddDefaultTableStoreSize computes and (if not already provided) adds the PEM flag for table store size.
func AddDefaultTableStoreSize(pemMemoryLimit string, customPEMFlags map[string]string) {
	// If the table store PEM flag is already set, we don't need to do anything.
	if _, ok := customPEMFlags[tableStoreSizePEMFlag]; ok {
		return
	}
	var defaultTableStoreSizeMB int
	if pemMemoryLimit == "" {
		defaultTableStoreSizeMB = defaultUncappedTableStoreSizeMB
	} else {
		pemMemorySizeBytes := resource.MustParse(pemMemoryLimit)
		defaultTableStoreSizeBytes := defaultTableStorePercentage * float64(pemMemorySizeBytes.Value())
		defaultTableStoreSizeMB = int(math.Floor(defaultTableStoreSizeBytes / bytesPerMiB))
		if defaultTableStoreSizeMB == 0 {
			log.Errorf("Default table store size must be nonzero, received total memory of %s", pemMemoryLimit)
			return
		}
	}
	customPEMFlags[tableStoreSizePEMFlag] = strconv.Itoa(defaultTableStoreSizeMB)
}

// GetConfigForVizier provides yaml names and content that can be used to deploy Vizier
func (s *Server) GetConfigForVizier(ctx context.Context,
	in *cpb.ConfigForVizierRequest) (*cpb.ConfigForVizierResponse, error) {
	log.Info("Fetching config for Vizier")

	templatedYAMLs, err := fetchVizierTemplates(ctx, "", in.VzSpec.Version, s.atClient)
	if err != nil {
		log.WithError(err).Error("Failed to fetch Vizier templates")
		return nil, err
	}

	// Fill in template values.
	cloudAddr := in.VzSpec.CloudAddr
	updateCloudAddr := in.VzSpec.CloudAddr
	if in.VzSpec.DevCloudNamespace != "" {
		cloudAddr = fmt.Sprintf("vzconn-service.%s.svc.cluster.local:51600", in.VzSpec.DevCloudNamespace)
		updateCloudAddr = fmt.Sprintf("api-service.%s.svc.cluster.local:51200", in.VzSpec.DevCloudNamespace)
	}

	// We should eventually clean up the templating code, since our Helm charts and extracted YAMLs will now just
	// be simple CRDs.
	tmplValues := &vizieryamls.VizierTmplValues{
		DeployKey:             in.VzSpec.DeployKey,
		CustomDeployKeySecret: in.VzSpec.CustomDeployKeySecret,
		UseEtcdOperator:       in.VzSpec.UseEtcdOperator,
		PEMMemoryLimit:        in.VzSpec.PemMemoryLimit,
		Namespace:             in.Namespace,
		CloudAddr:             cloudAddr,
		CloudUpdateAddr:       updateCloudAddr,
		ClusterName:           in.VzSpec.ClusterName,
		DisableAutoUpdate:     in.VzSpec.DisableAutoUpdate,
		SentryDSN:             getSentryDSN(in.VzSpec.Version),
		ClockConverter:        in.VzSpec.ClockConverter,
		DataAccess:            in.VzSpec.DataAccess,
	}

	if in.VzSpec.DataCollectorParams != nil && in.VzSpec.DataCollectorParams.DatastreamBufferSize != 0 {
		tmplValues.DatastreamBufferSize = in.VzSpec.DataCollectorParams.DatastreamBufferSize
	}
	if in.VzSpec.DataCollectorParams != nil && in.VzSpec.DataCollectorParams.DatastreamBufferSpikeSize != 0 {
		tmplValues.DatastreamBufferSpikeSize = in.VzSpec.DataCollectorParams.DatastreamBufferSpikeSize
	}
	if in.VzSpec.DataCollectorParams != nil && in.VzSpec.DataCollectorParams.CustomPEMFlags != nil {
		tmplValues.CustomPEMFlags = in.VzSpec.DataCollectorParams.CustomPEMFlags
	}
	if in.VzSpec.LeadershipElectionParams != nil {
		tmplValues.ElectionPeriodMs = in.VzSpec.LeadershipElectionParams.ElectionPeriodMs
	}

	// If the table store data limit is not specified, then we should add in the default
	// table store size. Default will be 60% of the total requested PEM memory.
	if tmplValues.CustomPEMFlags == nil {
		tmplValues.CustomPEMFlags = make(map[string]string)
	}
	AddDefaultTableStoreSize(tmplValues.PEMMemoryLimit, tmplValues.CustomPEMFlags)

	// Next we inject any feature flags that we want to set for this org.
	orgID, err := s.getOrgIDForDeployKey(tmplValues.DeployKey)
	if err != nil || orgID == uuid.Nil {
		log.WithError(err).Error("Error getting org ID from deploy key, skipping feature flag logic")
	} else {
		AddFeatureFlagsToTemplate(s.vzFeatureFlagClient, orgID, tmplValues)
	}

	vzYamls, err := yamls.ExecuteTemplatedYAMLs(templatedYAMLs, vizieryamls.VizierTmplValuesToArgs(tmplValues))
	if err != nil {
		log.WithError(err).Error("Failed to execute templates")
		return nil, err
	}

	// Apply custom patches, if any.
	if in.VzSpec.Patches != nil || len(in.VzSpec.Patches) > 0 {
		for _, y := range vzYamls {
			patchedYAML, err := yamls.AddPatchesToYAML(s.clientset, y.YAML, in.VzSpec.Patches, s.rm)
			if err != nil {
				log.WithError(err).Error("Failed to add patches")
				return nil, err
			}
			y.YAML = patchedYAML
		}
	}

	// Map from the YAML name to the YAML contents.
	yamlMap := make(map[string]string)
	for _, y := range vzYamls {
		yamlMap[y.Name] = y.YAML
	}

	return &cpb.ConfigForVizierResponse{
		NameToYamlContent: yamlMap,
		SentryDSN:         getSentryDSN(in.VzSpec.Version),
	}, nil
}

func getSentryDSN(vizierVersion string) string {
	// If it contains - it must be a pre-release Vizier.
	if strings.Contains(vizierVersion, "-") {
		return viper.GetString("dev_sentry")
	}
	return viper.GetString("prod_sentry")
}

// fetchVizierTemplates gets a download link, untars file, and
// converts to yaml maps.
func fetchVizierTemplates(ctx context.Context, authToken,
	versionStr string, atClient atpb.ArtifactTrackerClient) ([]*yamls.YAMLFile, error) {
	req := &atpb.GetDownloadLinkRequest{
		ArtifactName: "vizier",
		VersionStr:   versionStr,
		ArtifactType: versionspb.AT_CONTAINER_SET_TEMPLATE_YAMLS,
	}
	resp, err := atClient.GetDownloadLink(ctx, req)
	if err != nil {
		return nil, err
	}

	reader, err := downloadFile(resp.Url)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	yamlMap, err := tar.ReadTarFileFromReader(reader)
	if err != nil {
		return nil, err
	}

	// Convert to YAML files, using the provided file names.
	// Get the YAML names, in order.
	yamlNames := make([]string, len(yamlMap))
	i := 0
	for k := range yamlMap {
		yamlNames[i] = k
		i++
	}
	sort.Strings(yamlNames)

	// Write to YAMLFile slice.
	var yamlFiles []*yamls.YAMLFile
	re := regexp.MustCompile(`(?:[0-9]+_)(.*)(?:\.yaml)`)
	for _, fName := range yamlNames {
		// The filename looks like "./pixie_yamls/00_namespace.yaml", we want to extract the "namespace".
		ms := re.FindStringSubmatch(fName)
		if ms == nil || len(ms) != 2 {
			continue
		}
		yamlFiles = append(yamlFiles, &yamls.YAMLFile{
			Name: ms[1],
			YAML: yamlMap[fName],
		})
	}

	return yamlFiles, nil
}
