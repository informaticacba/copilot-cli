// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/copilot-cli/internal/pkg/addon"
	"github.com/aws/copilot-cli/internal/pkg/aws/cloudformation"
	"github.com/aws/copilot-cli/internal/pkg/aws/ecs"
	"github.com/aws/copilot-cli/internal/pkg/config"
	"github.com/aws/copilot-cli/internal/pkg/deploy"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation/stack"
	"github.com/aws/copilot-cli/internal/pkg/docker/dockerengine"
	"github.com/aws/copilot-cli/internal/pkg/manifest"
	"github.com/aws/copilot-cli/internal/pkg/term/color"
	"github.com/aws/copilot-cli/internal/pkg/term/log"

	"github.com/aws/copilot-cli/internal/pkg/cli/deploy/mocks"
)

type deployMocks struct {
	mockImageBuilderPusher     *mocks.MockimageBuilderPusher
	mockEndpointGetter         *mocks.MockendpointGetter
	mockSpinner                *mocks.Mockspinner
	mockPublicCIDRBlocksGetter *mocks.MockpublicCIDRBlocksGetter
	mockSNSTopicsLister        *mocks.MocksnsTopicsLister
	mockServiceDeployer        *mocks.MockserviceDeployer
	mockServiceForceUpdater    *mocks.MockserviceForceUpdater
	mockTemplater              *mocks.Mocktemplater
	mockUploader               *mocks.Mockuploader
	mockVersionGetter          *mocks.MockversionGetter
	mockFileReader             *mocks.MockfileReader
	mockValidator              *mocks.MockaliasCertValidator
}

type mockWorkloadMft struct {
	fileName      string
	buildRequired bool
}

func (m *mockWorkloadMft) EnvFile() string {
	return m.fileName
}

func (m *mockWorkloadMft) BuildRequired() (bool, error) {
	return m.buildRequired, nil
}

func (m *mockWorkloadMft) BuildArgs(rootDirectory string) *manifest.DockerBuildArgs {
	return &manifest.DockerBuildArgs{
		Dockerfile: aws.String("mockDockerfile"),
		Context:    aws.String("mockContext"),
	}
}

func (m *mockWorkloadMft) ContainerPlatform() string {
	return "mockContainerPlatform"
}

func TestWorkloadDeployer_UploadArtifacts(t *testing.T) {
	const (
		mockName            = "mockWkld"
		mockEnvName         = "test"
		mockAppName         = "press"
		mockWorkspacePath   = "."
		mockEnvFile         = "foo.env"
		mockS3Bucket        = "mockBucket"
		mockImageTag        = "mockImageTag"
		mockAddonsS3URL     = "https://mockS3DomainName/mockPath"
		mockBadEnvFileS3URL = "badURL"
		mockEnvFileS3URL    = "https://stackset-demo-infrastruc-pipelinebuiltartifactbuc-11dj7ctf52wyf.s3.us-west-2.amazonaws.com/manual/1638391936/env"
		mockEnvFileS3ARN    = "arn:aws:s3:::stackset-demo-infrastruc-pipelinebuiltartifactbuc-11dj7ctf52wyf/manual/1638391936/env"
	)
	mockResources := &stack.AppRegionalResources{
		S3Bucket: mockS3Bucket,
	}
	mockEnvFilePath := fmt.Sprintf("%s/%s/%s/%s.env", "manual", "env-files", mockEnvFile, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	mockAddonPath := fmt.Sprintf("%s/%s/%s/%s.yml", "manual", "addons", mockName, "1307990e6ba5ca145eb35e99182a9bec46531bc54ddf656a602c780fa0240dee")
	mockError := errors.New("some error")
	tests := map[string]struct {
		inEnvFile       string
		inBuildRequired bool
		inRegion        string

		mock func(m *deployMocks)

		wantAddonsURL     string
		wantEnvFileARN    string
		wantImageDigest   *string
		wantBuildRequired bool
		wantErr           error
	}{
		"error if failed to build and push image": {
			inBuildRequired: true,
			mock: func(m *deployMocks) {
				m.mockImageBuilderPusher.EXPECT().BuildAndPush(gomock.Any(), &dockerengine.BuildArguments{
					Dockerfile: "mockDockerfile",
					Context:    "mockContext",
					Platform:   "mockContainerPlatform",
					Tags:       []string{mockImageTag},
				}).Return("", mockError)
			},
			wantErr: fmt.Errorf("build and push image: some error"),
		},
		"build and push image successfully": {
			inBuildRequired: true,
			mock: func(m *deployMocks) {
				m.mockImageBuilderPusher.EXPECT().BuildAndPush(gomock.Any(), &dockerengine.BuildArguments{
					Dockerfile: "mockDockerfile",
					Context:    "mockContext",
					Platform:   "mockContainerPlatform",
					Tags:       []string{mockImageTag},
				}).Return("mockDigest", nil)
				m.mockTemplater.EXPECT().Template().Return("", &addon.ErrAddonsNotFound{
					WlName: "mockWkld",
				})
			},
			wantImageDigest: aws.String("mockDigest"),
		},
		"error if fail to read env file": {
			inEnvFile: mockEnvFile,
			mock: func(m *deployMocks) {
				m.mockFileReader.EXPECT().ReadFile(filepath.Join(mockWorkspacePath, mockEnvFile)).
					Return(nil, mockError)
			},
			wantErr: fmt.Errorf("read env file foo.env: some error"),
		},
		"error if fail to put env file to s3 bucket": {
			inEnvFile: mockEnvFile,
			mock: func(m *deployMocks) {
				m.mockFileReader.EXPECT().ReadFile(filepath.Join(mockWorkspacePath, mockEnvFile)).Return([]byte{}, nil)
				m.mockUploader.EXPECT().Upload(mockS3Bucket, mockEnvFilePath, gomock.Any()).
					Return("", mockError)
			},
			wantErr: fmt.Errorf("put env file foo.env artifact to bucket mockBucket: some error"),
		},
		"error if fail to parse s3 url": {
			inEnvFile: mockEnvFile,
			mock: func(m *deployMocks) {
				m.mockFileReader.EXPECT().ReadFile(filepath.Join(mockWorkspacePath, mockEnvFile)).Return([]byte{}, nil)
				m.mockUploader.EXPECT().Upload(mockS3Bucket, mockEnvFilePath, gomock.Any()).
					Return(mockBadEnvFileS3URL, nil)

			},
			wantErr: fmt.Errorf("parse s3 url: cannot parse S3 URL badURL into bucket name and key"),
		},
		"error if fail to find the partition": {
			inEnvFile: mockEnvFile,
			inRegion:  "sun-south-0",
			mock: func(m *deployMocks) {
				m.mockFileReader.EXPECT().ReadFile(filepath.Join(mockWorkspacePath, mockEnvFile)).Return([]byte{}, nil)
				m.mockUploader.EXPECT().Upload(mockS3Bucket, mockEnvFilePath, gomock.Any()).
					Return(mockEnvFileS3URL, nil)
			},
			wantErr: fmt.Errorf("find the partition for region sun-south-0"),
		},
		"should push addons template to S3 bucket": {
			inEnvFile: mockEnvFile,
			inRegion:  "us-west-2",
			mock: func(m *deployMocks) {
				m.mockFileReader.EXPECT().ReadFile(filepath.Join(mockWorkspacePath, mockEnvFile)).Return([]byte{}, nil)
				m.mockUploader.EXPECT().Upload(mockS3Bucket, mockEnvFilePath, gomock.Any()).
					Return(mockEnvFileS3URL, nil)
				m.mockTemplater.EXPECT().Template().Return("some data", nil)
				m.mockUploader.EXPECT().Upload(mockS3Bucket, mockAddonPath, gomock.Any()).
					Return(mockAddonsS3URL, nil)
			},

			wantAddonsURL:  mockAddonsS3URL,
			wantEnvFileARN: mockEnvFileS3ARN,
		},
		"should return error if fail to upload to S3 bucket": {
			inRegion: "us-west-2",
			mock: func(m *deployMocks) {
				m.mockTemplater.EXPECT().Template().Return("some data", nil)
				m.mockUploader.EXPECT().Upload(mockS3Bucket, mockAddonPath, gomock.Any()).
					Return("", mockError)
			},

			wantErr: fmt.Errorf("put addons artifact to bucket mockBucket: some error"),
		},
		"should return empty url if the service doesn't have any addons and env files": {
			mock: func(m *deployMocks) {
				m.mockTemplater.EXPECT().Template().Return("", &addon.ErrAddonsNotFound{
					WlName: "mockWkld",
				})
			},
		},
		"should fail if addons cannot be retrieved from workspace": {
			mock: func(m *deployMocks) {
				m.mockTemplater.EXPECT().Template().Return("", mockError)
			},
			wantErr: fmt.Errorf("retrieve addons template: %w", mockError),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			m := &deployMocks{
				mockUploader:           mocks.NewMockuploader(ctrl),
				mockTemplater:          mocks.NewMocktemplater(ctrl),
				mockImageBuilderPusher: mocks.NewMockimageBuilderPusher(ctrl),
				mockFileReader:         mocks.NewMockfileReader(ctrl),
			}
			tc.mock(m)

			deployer := workloadDeployer{
				name: mockName,
				env: &config.Environment{
					Name:   mockEnvName,
					Region: tc.inRegion,
				},
				app: &config.Application{
					Name: mockAppName,
				},
				resources:     mockResources,
				imageTag:      mockImageTag,
				workspacePath: mockWorkspacePath,
				mft: &mockWorkloadMft{
					fileName:      tc.inEnvFile,
					buildRequired: tc.inBuildRequired,
				},

				templater:          m.mockTemplater,
				fs:                 m.mockFileReader,
				s3Client:           m.mockUploader,
				imageBuilderPusher: m.mockImageBuilderPusher,
			}

			got, gotErr := deployer.UploadArtifacts()

			if tc.wantErr != nil {
				require.EqualError(t, gotErr, tc.wantErr.Error())
			} else {
				require.NoError(t, gotErr)
				require.Equal(t, tc.wantAddonsURL, got.AddonsURL)
				require.Equal(t, tc.wantEnvFileARN, got.EnvFileARN)
				require.Equal(t, tc.wantImageDigest, got.ImageDigest)
			}
		})
	}
}

func TestWorkloadDeployer_DeployWorkload(t *testing.T) {
	mockError := errors.New("some error")
	const (
		mockAppName  = "mockApp"
		mockEnvName  = "mockEnv"
		mockName     = "mockWkld"
		mockS3Bucket = "mockBucket"
	)
	mockAliases := []string{"example.com", "foobar.com"}
	mockCertARNs := []string{"mockCertARN"}
	mockResources := &stack.AppRegionalResources{
		S3Bucket: mockS3Bucket,
	}
	mockNowTime := time.Unix(1494505750, 0)
	mockBeforeTime := time.Unix(1494505743, 0)
	mockAfterTime := time.Unix(1494505756, 0)
	tests := map[string]struct {
		inAliases         manifest.Alias
		inNLB             manifest.NetworkLoadBalancerConfiguration
		inApp             *config.Application
		inEnvironment     *config.Environment
		inForceDeploy     bool
		inDisableRollback bool

		mock func(m *deployMocks)

		wantErr error
	}{
		"fail to get service discovery endpoint": {
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("", mockError)
			},
			wantErr: fmt.Errorf("get service discovery endpoint: some error"),
		},
		"fail if alias is not specified with env has imported certs": {
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
				CustomConfig: &config.CustomizeEnv{
					ImportCertARNs: mockCertARNs,
				},
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},
			wantErr: fmt.Errorf("cannot deploy service mockWkld without http.alias to environment mockEnv with certificate imported"),
		},
		"fail to validate certificate aliases": {
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
				CustomConfig: &config.CustomizeEnv{
					ImportCertARNs: mockCertARNs,
				},
			},
			inAliases: manifest.Alias{
				StringSlice: mockAliases,
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockValidator.EXPECT().ValidateCertAliases(mockAliases, mockCertARNs).Return(mockError)
			},
			wantErr: fmt.Errorf("validate aliases against the imported certificate for env mockEnv: some error"),
		},
		"fail to get public CIDR blocks": {
			inNLB: manifest.NetworkLoadBalancerConfiguration{
				Port: aws.String("443/tcp"),
			},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockPublicCIDRBlocksGetter.EXPECT().PublicCIDRBlocks().Return(nil, errors.New("some error"))
			},
			wantErr: fmt.Errorf("get public CIDR blocks information from the VPC of environment mockEnv: some error"),
		},
		"alias used while app is not associated with a domain": {
			inAliases: manifest.Alias{String: aws.String("mockAlias")},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},
			wantErr: errors.New("cannot specify http.alias when application is not associated with a domain and env mockEnv doesn't import one or more certificates"),
		},
		"nlb alias used while app is not associated with a domain": {
			inNLB: manifest.NetworkLoadBalancerConfiguration{
				Port:    aws.String("80"),
				Aliases: manifest.Alias{String: aws.String("mockAlias")},
			},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},
			wantErr: errors.New("cannot specify nlb.alias when application is not associated with a domain"),
		},
		"nlb alias used while env has imported certs": {
			inAliases: manifest.Alias{String: aws.String("mockAlias")},
			inNLB: manifest.NetworkLoadBalancerConfiguration{
				Port:    aws.String("80"),
				Aliases: manifest.Alias{String: aws.String("mockAlias")},
			},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
				CustomConfig: &config.CustomizeEnv{
					ImportCertARNs: mockCertARNs,
				},
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockValidator.EXPECT().ValidateCertAliases([]string{"mockAlias"}, mockCertARNs).Return(nil)
			},
			wantErr: errors.New("cannot specify nlb.alias when env mockEnv imports one or more certificates"),
		},
		"fail to get app version": {
			inAliases: manifest.Alias{String: aws.String("mockAlias")},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("", mockError)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},
			wantErr: fmt.Errorf("get version for app %s: %w", mockAppName, mockError),
		},
		"fail to enable https alias because of incompatible app version": {
			inAliases: manifest.Alias{String: aws.String("mockAlias")},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v0.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},
			wantErr: fmt.Errorf("alias is not compatible with application versions below %s", deploy.AliasLeastAppTemplateVersion),
		},
		"fail to enable nlb alias because of incompatible app version": {
			inNLB: manifest.NetworkLoadBalancerConfiguration{
				Port:    aws.String("80"),
				Aliases: manifest.Alias{String: aws.String("mockAlias")},
			},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v0.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},
			wantErr: fmt.Errorf("alias is not compatible with application versions below %s", deploy.AliasLeastAppTemplateVersion),
		},
		"fail to enable https alias because of invalid alias": {
			inAliases: manifest.Alias{String: aws.String("v1.v2.mockDomain")},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v1.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},
			wantErr: fmt.Errorf(`alias "v1.v2.mockDomain" is not supported in hosted zones managed by Copilot`),
		},
		"fail to enable nlb alias because of invalid alias": {
			inNLB: manifest.NetworkLoadBalancerConfiguration{
				Port:    aws.String("80"),
				Aliases: manifest.Alias{String: aws.String("v1.v2.mockDomain")},
			},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v1.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},
			wantErr: fmt.Errorf(`alias "v1.v2.mockDomain" is not supported in hosted zones managed by Copilot`),
		},
		"error if fail to deploy service": {
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockServiceDeployer.EXPECT().DeployService(gomock.Any(), gomock.Any(), "mockBucket", gomock.Any()).Return(errors.New("some error"))
			},
			wantErr: fmt.Errorf("deploy service: some error"),
		},
		"error if change set is empty but force flag is not set": {
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockServiceDeployer.EXPECT().DeployService(gomock.Any(), gomock.Any(), "mockBucket", gomock.Any()).Return(cloudformation.NewMockErrChangeSetEmpty())
			},
			wantErr: fmt.Errorf("deploy service: change set with name mockChangeSet for stack mockStack has no changes"),
		},
		"error if fail to get last update time when force an update": {
			inForceDeploy: true,
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockServiceDeployer.EXPECT().DeployService(gomock.Any(), gomock.Any(), "mockBucket", gomock.Any()).
					Return(nil)
				m.mockServiceForceUpdater.EXPECT().LastUpdatedAt(mockAppName, mockEnvName, mockName).
					Return(time.Time{}, mockError)
			},
			wantErr: fmt.Errorf("get the last updated deployment time for mockWkld: some error"),
		},
		"skip force updating when cmd run time is after the last update time": {
			inForceDeploy: true,
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockServiceDeployer.EXPECT().DeployService(gomock.Any(), gomock.Any(), "mockBucket", gomock.Any()).
					Return(nil)
				m.mockServiceForceUpdater.EXPECT().LastUpdatedAt(mockAppName, mockEnvName, mockName).
					Return(mockAfterTime, nil)
			},
		},
		"error if fail to force an update": {
			inForceDeploy: true,
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockServiceDeployer.EXPECT().DeployService(gomock.Any(), gomock.Any(), "mockBucket", gomock.Any()).
					Return(cloudformation.NewMockErrChangeSetEmpty())
				m.mockServiceForceUpdater.EXPECT().LastUpdatedAt(mockAppName, mockEnvName, mockName).
					Return(mockBeforeTime, nil)
				m.mockSpinner.EXPECT().Start(fmt.Sprintf(fmtForceUpdateSvcStart, mockName, mockEnvName))
				m.mockServiceForceUpdater.EXPECT().ForceUpdateService(mockAppName, mockEnvName, mockName).Return(mockError)
				m.mockSpinner.EXPECT().Stop(log.Serrorf(fmtForceUpdateSvcFailed, mockName, mockEnvName, mockError))
			},
			wantErr: fmt.Errorf("force an update for service mockWkld: some error"),
		},
		"error if fail to force an update because of timeout": {
			inForceDeploy: true,
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockServiceDeployer.EXPECT().DeployService(gomock.Any(), gomock.Any(), "mockBucket", gomock.Any()).
					Return(cloudformation.NewMockErrChangeSetEmpty())
				m.mockServiceForceUpdater.EXPECT().LastUpdatedAt(mockAppName, mockEnvName, mockName).
					Return(mockBeforeTime, nil)
				m.mockSpinner.EXPECT().Start(fmt.Sprintf(fmtForceUpdateSvcStart, mockName, mockEnvName))
				m.mockServiceForceUpdater.EXPECT().ForceUpdateService(mockAppName, mockEnvName, mockName).
					Return(&ecs.ErrWaitServiceStableTimeout{})
				m.mockSpinner.EXPECT().Stop(
					log.Serror(fmt.Sprintf("%s  Run %s to check for the fail reason.\n",
						fmt.Sprintf(fmtForceUpdateSvcFailed, mockName, mockEnvName, &ecs.ErrWaitServiceStableTimeout{}),
						color.HighlightCode(fmt.Sprintf("copilot svc status --name %s --env %s", mockName, mockEnvName)))))
			},
			wantErr: fmt.Errorf("force an update for service mockWkld: max retries 0 exceeded"),
		},
		"skip validating": {
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockServiceDeployer.EXPECT().DeployService(gomock.Any(), gomock.Any(), "mockBucket", gomock.Any()).Return(nil)
			},
		},
		"success": {
			inAliases: manifest.Alias{
				StringSlice: mockAliases,
			},
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
				CustomConfig: &config.CustomizeEnv{
					ImportCertARNs: mockCertARNs,
				},
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockValidator.EXPECT().ValidateCertAliases(mockAliases, mockCertARNs).Return(nil)
				m.mockServiceDeployer.EXPECT().DeployService(gomock.Any(), gomock.Any(), "mockBucket", gomock.Any()).Return(nil)
			},
		},
		"success with force update": {
			inForceDeploy: true,
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockServiceDeployer.EXPECT().DeployService(gomock.Any(), gomock.Any(), "mockBucket", gomock.Any()).
					Return(cloudformation.NewMockErrChangeSetEmpty())
				m.mockServiceForceUpdater.EXPECT().LastUpdatedAt(mockAppName, mockEnvName, mockName).
					Return(mockBeforeTime, nil)
				m.mockSpinner.EXPECT().Start(fmt.Sprintf(fmtForceUpdateSvcStart, mockName, mockEnvName))
				m.mockServiceForceUpdater.EXPECT().ForceUpdateService(mockAppName, mockEnvName, mockName).Return(nil)
				m.mockSpinner.EXPECT().Stop(log.Ssuccessf(fmtForceUpdateSvcComplete, mockName, mockEnvName))
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			m := &deployMocks{
				mockVersionGetter:          mocks.NewMockversionGetter(ctrl),
				mockEndpointGetter:         mocks.NewMockendpointGetter(ctrl),
				mockServiceDeployer:        mocks.NewMockserviceDeployer(ctrl),
				mockServiceForceUpdater:    mocks.NewMockserviceForceUpdater(ctrl),
				mockSpinner:                mocks.NewMockspinner(ctrl),
				mockPublicCIDRBlocksGetter: mocks.NewMockpublicCIDRBlocksGetter(ctrl),
				mockValidator:              mocks.NewMockaliasCertValidator(ctrl),
			}
			tc.mock(m)

			deployer := lbSvcDeployer{
				svcDeployer: &svcDeployer{
					workloadDeployer: &workloadDeployer{
						name:           mockName,
						app:            tc.inApp,
						env:            tc.inEnvironment,
						resources:      mockResources,
						deployer:       m.mockServiceDeployer,
						endpointGetter: m.mockEndpointGetter,
						spinner:        m.mockSpinner,
					},
					newSvcUpdater: func(f func(*session.Session) serviceForceUpdater) serviceForceUpdater {
						return m.mockServiceForceUpdater
					},
					now: func() time.Time {
						return mockNowTime
					},
				},
				appVersionGetter:       m.mockVersionGetter,
				publicCIDRBlocksGetter: m.mockPublicCIDRBlocksGetter,
				aliasCertValidator:     m.mockValidator,
				lbMft: &manifest.LoadBalancedWebService{
					Workload: manifest.Workload{
						Name: aws.String(mockName),
					},
					LoadBalancedWebServiceConfig: manifest.LoadBalancedWebServiceConfig{
						ImageConfig: manifest.ImageWithPortAndHealthcheck{
							ImageWithPort: manifest.ImageWithPort{
								Image: manifest.Image{
									Build: manifest.BuildArgsOrString{BuildString: aws.String("/Dockerfile")},
								},
								Port: aws.Uint16(80),
							},
						},
						RoutingRule: manifest.RoutingRuleConfigOrBool{
							RoutingRuleConfiguration: manifest.RoutingRuleConfiguration{
								Path:  aws.String("/"),
								Alias: tc.inAliases,
							},
						},
						NLBConfig: tc.inNLB,
					},
				},
			}

			_, gotErr := deployer.DeployWorkload(&DeployWorkloadInput{
				Options: Options{
					ForceNewUpdate:  tc.inForceDeploy,
					DisableRollback: tc.inDisableRollback,
				},
			})

			if tc.wantErr != nil {
				require.EqualError(t, gotErr, tc.wantErr.Error())
			} else {
				require.NoError(t, gotErr)
			}
		})
	}
}

type deployRDSvcMocks struct {
	mockVersionGetter  *mocks.MockversionGetter
	mockEndpointGetter *mocks.MockendpointGetter
	mockUploader       *mocks.MockcustomResourcesUploader
}

func TestSvcDeployOpts_rdWebServiceStackConfiguration(t *testing.T) {
	const (
		mockAppName   = "mockApp"
		mockEnvName   = "mockEnv"
		mockName      = "mockWkld"
		mockAddonsURL = "mockAddonsURL"
		mockBucket    = "mockBucket"
	)
	mockResources := &stack.AppRegionalResources{
		S3Bucket: mockBucket,
	}
	tests := map[string]struct {
		inAlias       string
		inApp         *config.Application
		inEnvironment *config.Environment

		mock func(m *deployRDSvcMocks)

		wantAlias string
		wantErr   error
	}{
		"alias used while app is not associated with a domain": {
			inAlias: "v1.mockDomain",
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name: mockAppName,
			},
			mock: func(m *deployRDSvcMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},

			wantErr: errors.New("alias specified when application is not associated with a domain"),
		},
		"invalid alias with unknown domain": {
			inAlias: "v1.someRandomDomain",
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployRDSvcMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v1.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},

			wantErr: fmt.Errorf("alias is not supported in hosted zones that are not managed by Copilot"),
		},
		"invalid environment level alias": {
			inAlias: "mockEnv.mockApp.mockDomain",
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployRDSvcMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v1.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},

			wantErr: fmt.Errorf("mockEnv.mockApp.mockDomain is an environment-level alias, which is not supported yet"),
		},
		"invalid application level alias": {
			inAlias: "someSub.mockApp.mockDomain",
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployRDSvcMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v1.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},

			wantErr: fmt.Errorf("someSub.mockApp.mockDomain is an application-level alias, which is not supported yet"),
		},
		"invalid root level alias": {
			inAlias: "mockDomain",
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployRDSvcMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v1.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
			},

			wantErr: fmt.Errorf("mockDomain is a root domain alias, which is not supported yet"),
		},
		"fail to upload custom resource scripts": {
			inAlias: "v1.mockDomain",
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployRDSvcMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v1.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockUploader.EXPECT().UploadRequestDrivenWebServiceCustomResources(gomock.Any()).Return(nil, errors.New("some error"))
			},

			wantErr: fmt.Errorf("upload custom resources to bucket mockBucket: some error"),
		},
		"success": {
			inAlias: "v1.mockDomain",
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployRDSvcMocks) {
				m.mockVersionGetter.EXPECT().Version().Return("v1.0.0", nil)
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockUploader.EXPECT().UploadRequestDrivenWebServiceCustomResources(gomock.Any()).Return(map[string]string{
					"mockResource2": "mockURL2",
				}, nil)
			},
			wantAlias: "v1.mockDomain",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			m := &deployRDSvcMocks{
				mockVersionGetter:  mocks.NewMockversionGetter(ctrl),
				mockEndpointGetter: mocks.NewMockendpointGetter(ctrl),
				mockUploader:       mocks.NewMockcustomResourcesUploader(ctrl),
			}
			tc.mock(m)

			deployer := rdwsDeployer{
				svcDeployer: &svcDeployer{
					workloadDeployer: &workloadDeployer{
						name:           mockName,
						app:            tc.inApp,
						env:            tc.inEnvironment,
						resources:      mockResources,
						endpointGetter: m.mockEndpointGetter,
					},
					newSvcUpdater: func(f func(*session.Session) serviceForceUpdater) serviceForceUpdater {
						return nil
					},
				},
				customResourceUploader: m.mockUploader,
				appVersionGetter:       m.mockVersionGetter,
				rdwsMft: &manifest.RequestDrivenWebService{
					Workload: manifest.Workload{
						Name: aws.String(mockName),
					},
					RequestDrivenWebServiceConfig: manifest.RequestDrivenWebServiceConfig{
						ImageConfig: manifest.ImageWithPort{
							Image: manifest.Image{
								Build: manifest.BuildArgsOrString{BuildString: aws.String("/Dockerfile")},
							},
							Port: aws.Uint16(80),
						},
						RequestDrivenWebServiceHttpConfig: manifest.RequestDrivenWebServiceHttpConfig{
							Alias: aws.String(tc.inAlias),
						},
					},
				},
			}

			got, gotErr := deployer.stackConfiguration(&StackRuntimeConfiguration{
				AddonsURL: mockAddonsURL,
			})

			if tc.wantErr != nil {
				require.EqualError(t, gotErr, tc.wantErr.Error())
			} else {
				require.NoError(t, gotErr)
				require.Equal(t, tc.wantAlias, got.rdSvcAlias)
			}
		})
	}
}

func TestSvcDeployOpts_stackConfiguration_worker(t *testing.T) {
	mockError := errors.New("some error")
	topic, _ := deploy.NewTopic("arn:aws:sns:us-west-2:0123456789012:mockApp-mockEnv-mockwkld-givesdogs", "mockApp", "mockEnv", "mockwkld")
	const (
		mockAppName = "mockApp"
		mockEnvName = "mockEnv"
		mockName    = "mockwkld"
		mockBucket  = "mockBucket"
	)
	mockResources := &stack.AppRegionalResources{
		S3Bucket: mockBucket,
	}
	mockTopics := []manifest.TopicSubscription{
		{
			Name:    aws.String("givesdogs"),
			Service: aws.String("mockwkld"),
		},
	}
	tests := map[string]struct {
		inAlias        string
		inApp          *config.Application
		inEnvironment  *config.Environment
		inBuildRequire bool

		mock func(m *deployMocks)

		wantErr             error
		wantedSubscriptions []manifest.TopicSubscription
	}{
		"fail to get deployed topics": {
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockApp.local", nil)
				m.mockSNSTopicsLister.EXPECT().ListSNSTopics(mockAppName, mockEnvName).Return(nil, mockError)
			},
			wantErr: fmt.Errorf("get SNS topics for app mockApp and environment mockEnv: %w", mockError),
		},
		"success": {
			inEnvironment: &config.Environment{
				Name:   mockEnvName,
				Region: "us-west-2",
			},
			inApp: &config.Application{
				Name:   mockAppName,
				Domain: "mockDomain",
			},
			mock: func(m *deployMocks) {
				m.mockEndpointGetter.EXPECT().ServiceDiscoveryEndpoint().Return("mockEnv.mockApp.local", nil)
				m.mockSNSTopicsLister.EXPECT().ListSNSTopics(mockAppName, mockEnvName).Return([]deploy.Topic{
					*topic,
				}, nil)
			},
			wantedSubscriptions: mockTopics,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := &deployMocks{
				mockEndpointGetter:  mocks.NewMockendpointGetter(ctrl),
				mockSNSTopicsLister: mocks.NewMocksnsTopicsLister(ctrl),
			}
			tc.mock(m)

			deployer := workerSvcDeployer{
				svcDeployer: &svcDeployer{
					workloadDeployer: &workloadDeployer{
						name:           mockName,
						app:            tc.inApp,
						env:            tc.inEnvironment,
						resources:      mockResources,
						endpointGetter: m.mockEndpointGetter,
					},
					newSvcUpdater: func(f func(*session.Session) serviceForceUpdater) serviceForceUpdater {
						return nil
					},
				},
				topicLister: m.mockSNSTopicsLister,
				wsMft: &manifest.WorkerService{
					Workload: manifest.Workload{
						Name: aws.String(mockName),
					},
					WorkerServiceConfig: manifest.WorkerServiceConfig{
						ImageConfig: manifest.ImageWithHealthcheck{
							Image: manifest.Image{
								Build: manifest.BuildArgsOrString{BuildString: aws.String("/Dockerfile")},
							},
						},
						Subscribe: manifest.SubscribeConfig{
							Topics: mockTopics,
						},
					},
				},
			}

			got, gotErr := deployer.stackConfiguration(&StackRuntimeConfiguration{})

			if tc.wantErr != nil {
				require.EqualError(t, gotErr, tc.wantErr.Error())
			} else {
				require.NoError(t, gotErr)
				require.ElementsMatch(t, tc.wantedSubscriptions, got.subscriptions)
			}
		})
	}
}

func Test_validateTopicsExist(t *testing.T) {
	mockApp := "app"
	mockEnv := "env"
	mockAllowedTopics := []string{
		"arn:aws:sqs:us-west-2:123456789012:app-env-database-events",
		"arn:aws:sqs:us-west-2:123456789012:app-env-database-orders",
		"arn:aws:sqs:us-west-2:123456789012:app-env-api-events",
	}
	duration10Hours := 10 * time.Hour
	testGoodTopics := []manifest.TopicSubscription{
		{
			Name:    aws.String("events"),
			Service: aws.String("database"),
		},
		{
			Name:    aws.String("orders"),
			Service: aws.String("database"),
			Queue: manifest.SQSQueueOrBool{
				Advanced: manifest.SQSQueue{
					Retention: &duration10Hours,
				},
			},
		},
	}
	testCases := map[string]struct {
		inTopics    []manifest.TopicSubscription
		inTopicARNs []string

		wantErr string
	}{
		"empty subscriptions": {
			inTopics:    nil,
			inTopicARNs: mockAllowedTopics,
		},
		"topics are valid": {
			inTopics:    testGoodTopics,
			inTopicARNs: mockAllowedTopics,
		},
		"topic is invalid": {
			inTopics:    testGoodTopics,
			inTopicARNs: []string{},
			wantErr:     "SNS topic app-env-database-events does not exist in environment env",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			err := validateTopicsExist(tc.inTopics, tc.inTopicARNs, mockApp, mockEnv)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
