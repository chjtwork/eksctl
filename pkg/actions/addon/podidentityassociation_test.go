package addon_test

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/smithy-go"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/stretchr/testify/mock"

	"github.com/weaveworks/eksctl/pkg/actions/addon"
	"github.com/weaveworks/eksctl/pkg/actions/addon/mocks"
	"github.com/weaveworks/eksctl/pkg/actions/podidentityassociation/fakes"
	piamocks "github.com/weaveworks/eksctl/pkg/actions/podidentityassociation/mocks"
	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/cfn/manager"
	managerfakes "github.com/weaveworks/eksctl/pkg/cfn/manager/fakes"
	"github.com/weaveworks/eksctl/pkg/eks/mocksv2"
	"github.com/weaveworks/eksctl/pkg/testutils/mockprovider"
)

var _ = Describe("Update Pod Identity Association", func() {
	type piaMocks struct {
		stackManager   *fakes.FakeStackUpdater
		stackDescriber *piamocks.StackDescriber
		roleCreator    *mocks.IAMRoleCreator
		roleUpdater    *mocks.IAMRoleUpdater
		eks            *mocksv2.EKS
	}
	type updateEntry struct {
		podIdentityAssociations []api.PodIdentityAssociation
		mockCalls               func(m piaMocks)

		expectedCalls                        func(stackManager *managerfakes.FakeStackManager, eksAPI *mocksv2.EKS)
		expectedAddonPodIdentityAssociations []ekstypes.AddonPodIdentityAssociations

		expectedErr string
	}

	const clusterName = "test"

	makeID := func(i int) string {
		return fmt.Sprintf("a-%d", i+1)
	}
	type listPodIdentityInput struct {
		namespace      string
		serviceAccount string
	}
	defaultListPodIdentityInputs := []listPodIdentityInput{
		{
			namespace:      "kube-system",
			serviceAccount: "vpc-cni",
		},
		{
			namespace:      "kube-system",
			serviceAccount: "aws-ebs-csi-driver",
		},
		{
			namespace:      "karpenter",
			serviceAccount: "karpenter",
		},
	}
	mockListPodIdentityAssociations := func(eksAPI *mocksv2.EKS, hasAssociation bool, listInputs []listPodIdentityInput) {
		for i, listInput := range listInputs {
			var associations []ekstypes.PodIdentityAssociationSummary
			if hasAssociation {
				associations = []ekstypes.PodIdentityAssociationSummary{
					{
						Namespace:      aws.String(listInput.namespace),
						ServiceAccount: aws.String(listInput.serviceAccount),
						AssociationId:  aws.String(makeID(i)),
					},
				}
			}
			eksAPI.On("ListPodIdentityAssociations", mock.Anything, &eks.ListPodIdentityAssociationsInput{
				ClusterName:    aws.String(clusterName),
				Namespace:      aws.String(listInput.namespace),
				ServiceAccount: aws.String(listInput.serviceAccount),
			}).Return(&eks.ListPodIdentityAssociationsOutput{
				Associations: associations,
			}, nil).Once()
		}
	}

	mockDescribePodIdentityAssociation := func(eksAPI *mocksv2.EKS, roleARNs ...string) {
		for i, roleARN := range roleARNs {
			id := aws.String(makeID(i))
			eksAPI.On("DescribePodIdentityAssociation", mock.Anything, &eks.DescribePodIdentityAssociationInput{
				ClusterName:   aws.String(clusterName),
				AssociationId: id,
			}).Return(&eks.DescribePodIdentityAssociationOutput{
				Association: &ekstypes.PodIdentityAssociation{
					AssociationId: id,
					RoleArn:       aws.String(roleARN),
				},
			}, nil).Once()
		}
	}

	DescribeTable("update pod identity association", func(e updateEntry) {
		provider := mockprovider.NewMockProvider()
		var (
			roleCreator    mocks.IAMRoleCreator
			roleUpdater    mocks.IAMRoleUpdater
			stackUpdater   fakes.FakeStackUpdater
			stackDescriber piamocks.StackDescriber
		)

		piaUpdater := &addon.PodIdentityAssociationUpdater{
			ClusterName:             clusterName,
			IAMRoleCreator:          &roleCreator,
			IAMRoleUpdater:          &roleUpdater,
			EKSPodIdentityDescriber: provider.MockEKS(),
			StackDescriber:          &stackDescriber,
		}
		if e.mockCalls != nil {
			e.mockCalls(piaMocks{
				stackManager:   &stackUpdater,
				stackDescriber: &stackDescriber,
				roleCreator:    &roleCreator,
				roleUpdater:    &roleUpdater,
				eks:            provider.MockEKS(),
			})
		}
		addonPodIdentityAssociations, err := piaUpdater.UpdateRole(context.Background(), e.podIdentityAssociations, "")
		if e.expectedErr != "" {
			Expect(err).To(MatchError(ContainSubstring(e.expectedErr)))
			return
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(addonPodIdentityAssociations).To(Equal(e.expectedAddonPodIdentityAssociations))
		t := GinkgoT()
		roleCreator.AssertExpectations(t)
		roleUpdater.AssertExpectations(t)
		provider.MockEKS().AssertExpectations(t)
	},
		Entry("addon contains pod identity that does not exist", updateEntry{
			podIdentityAssociations: []api.PodIdentityAssociation{
				{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
				},
			},
			mockCalls: func(m piaMocks) {
				m.eks.On("ListPodIdentityAssociations", mock.Anything, &eks.ListPodIdentityAssociationsInput{
					ClusterName:    aws.String(clusterName),
					Namespace:      aws.String("kube-system"),
					ServiceAccount: aws.String("vpc-cni"),
				}).Return(&eks.ListPodIdentityAssociationsOutput{}, nil)

				m.roleCreator.On("Create", mock.Anything, &api.PodIdentityAssociation{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
				}, "").Return("role-1", nil)

			},
			expectedAddonPodIdentityAssociations: []ekstypes.AddonPodIdentityAssociations{
				{
					ServiceAccount: aws.String("vpc-cni"),
					RoleArn:        aws.String("role-1"),
				},
			},
		}),

		Entry("addon contains pod identities, some of which do not exist", updateEntry{
			podIdentityAssociations: []api.PodIdentityAssociation{
				{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
				},
				{
					Namespace:          "kube-system",
					ServiceAccountName: "aws-ebs-csi-driver",
				},
				{
					Namespace:          "karpenter",
					ServiceAccountName: "karpenter",
				},
			},
			mockCalls: func(m piaMocks) {
				mockListPodIdentityAssociations(m.eks, true, []listPodIdentityInput{
					{
						namespace:      "kube-system",
						serviceAccount: "vpc-cni",
					},
				})
				mockDescribePodIdentityAssociation(m.eks, "cni-role")
				mockListPodIdentityAssociations(m.eks, false, []listPodIdentityInput{
					{
						namespace:      "kube-system",
						serviceAccount: "aws-ebs-csi-driver",
					},
					{
						namespace:      "karpenter",
						serviceAccount: "karpenter",
					},
				})

				m.roleUpdater.On("Update", mock.Anything, api.PodIdentityAssociation{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
				}, "eksctl-test-addon--podidentityrole-vpc-cni", "a-1").Return("cni-role-2", true, nil).Once()
				m.stackDescriber.On("DescribeStack", mock.Anything, &manager.Stack{
					StackName: aws.String("eksctl-test-addon--podidentityrole-vpc-cni"),
				}).Return(&manager.Stack{}, nil)

				m.roleCreator.On("Create", mock.Anything, &api.PodIdentityAssociation{
					Namespace:          "kube-system",
					ServiceAccountName: "aws-ebs-csi-driver",
				}, "").Return("csi-role", nil).Once()
				m.roleCreator.On("Create", mock.Anything, &api.PodIdentityAssociation{
					Namespace:          "karpenter",
					ServiceAccountName: "karpenter",
				}, "").Return("karpenter-role", nil).Once()
			},
			expectedAddonPodIdentityAssociations: []ekstypes.AddonPodIdentityAssociations{
				{
					ServiceAccount: aws.String("vpc-cni"),
					RoleArn:        aws.String("cni-role-2"),
				},
				{
					ServiceAccount: aws.String("aws-ebs-csi-driver"),
					RoleArn:        aws.String("csi-role"),
				},
				{
					ServiceAccount: aws.String("karpenter"),
					RoleArn:        aws.String("karpenter-role"),
				},
			},
		}),

		Entry("addon contains pod identities that already exist", updateEntry{
			podIdentityAssociations: []api.PodIdentityAssociation{
				{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
				},
				{
					Namespace:          "kube-system",
					ServiceAccountName: "aws-ebs-csi-driver",
				},
				{
					Namespace:          "karpenter",
					ServiceAccountName: "karpenter",
				},
			},
			mockCalls: func(m piaMocks) {

				// TODO:
				//m.stackManager.DescribeStackStub
				mockListPodIdentityAssociations(m.eks, true, defaultListPodIdentityInputs)
				mockDescribePodIdentityAssociation(m.eks, "cni-role", "csi-role", "karpenter-role")

				for i, updateInput := range []struct {
					namespace            string
					serviceAccount       string
					hasIAMResourcesStack bool
					stackName            string
					returnRole           string
				}{
					{
						namespace:            "kube-system",
						serviceAccount:       "vpc-cni",
						hasIAMResourcesStack: true,
						stackName:            "kube-system-vpc-cni",
						returnRole:           "cni-role-2",
					},
					{
						namespace:            "kube-system",
						serviceAccount:       "aws-ebs-csi-driver",
						hasIAMResourcesStack: true,
						stackName:            "kube-system-aws-ebs-csi-driver",
						returnRole:           "csi-role-2",
					},
					{
						namespace:            "karpenter",
						serviceAccount:       "karpenter",
						hasIAMResourcesStack: true,
						stackName:            "karpenter-karpenter",
						returnRole:           "karpenter-role-2",
					},
				} {
					id := makeID(i)

					stackName := fmt.Sprintf("eksctl-test-addon--podidentityrole-%s", updateInput.serviceAccount)
					m.roleUpdater.On("Update", mock.Anything, api.PodIdentityAssociation{
						Namespace:          updateInput.namespace,
						ServiceAccountName: updateInput.serviceAccount,
					}, stackName, id).Return(updateInput.returnRole, true, nil).Once()

					m.stackDescriber.On("DescribeStack", mock.Anything, &manager.Stack{
						StackName: aws.String(stackName),
					}).Return(&manager.Stack{
						StackName: aws.String(stackName),
					}, nil)
				}
			},
			expectedAddonPodIdentityAssociations: []ekstypes.AddonPodIdentityAssociations{
				{
					ServiceAccount: aws.String("vpc-cni"),
					RoleArn:        aws.String("cni-role-2"),
				},
				{
					ServiceAccount: aws.String("aws-ebs-csi-driver"),
					RoleArn:        aws.String("csi-role-2"),
				},
				{
					ServiceAccount: aws.String("karpenter"),
					RoleArn:        aws.String("karpenter-role-2"),
				},
			},
		}),

		Entry("addon contains pod identities that do not exist and have a pre-existing roleARN", updateEntry{
			podIdentityAssociations: []api.PodIdentityAssociation{
				{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
					RoleARN:            "role-1",
				},
				{
					Namespace:          "kube-system",
					ServiceAccountName: "aws-ebs-csi-driver",
					RoleARN:            "role-2",
				},
				{
					Namespace:          "karpenter",
					ServiceAccountName: "karpenter",
					RoleARN:            "role-3",
				},
			},
			mockCalls: func(m piaMocks) {
				mockListPodIdentityAssociations(m.eks, false, defaultListPodIdentityInputs)
			},
			expectedAddonPodIdentityAssociations: []ekstypes.AddonPodIdentityAssociations{
				{
					ServiceAccount: aws.String("vpc-cni"),
					RoleArn:        aws.String("role-1"),
				},
				{
					ServiceAccount: aws.String("aws-ebs-csi-driver"),
					RoleArn:        aws.String("role-2"),
				},
				{
					ServiceAccount: aws.String("karpenter"),
					RoleArn:        aws.String("role-3"),
				},
			},
		}),

		Entry("addon contains pod identities that already exist and have a pre-existing roleARN", updateEntry{
			podIdentityAssociations: []api.PodIdentityAssociation{
				{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
					RoleARN:            "role-1",
				},
				{
					Namespace:          "kube-system",
					ServiceAccountName: "aws-ebs-csi-driver",
					RoleARN:            "role-2",
				},
				{
					Namespace:          "karpenter",
					ServiceAccountName: "karpenter",
					RoleARN:            "role-3",
				},
			},
			mockCalls: func(m piaMocks) {
				mockListPodIdentityAssociations(m.eks, false, defaultListPodIdentityInputs)

			},
			expectedAddonPodIdentityAssociations: []ekstypes.AddonPodIdentityAssociations{
				{
					ServiceAccount: aws.String("vpc-cni"),
					RoleArn:        aws.String("role-1"),
				},
				{
					ServiceAccount: aws.String("aws-ebs-csi-driver"),
					RoleArn:        aws.String("role-2"),
				},
				{
					ServiceAccount: aws.String("karpenter"),
					RoleArn:        aws.String("role-3"),
				},
			},
		}),

		Entry("addon contains pod identity IAM resources created by eksctl but are being updated with a new roleARN", updateEntry{
			podIdentityAssociations: []api.PodIdentityAssociation{
				{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
					RoleARN:            "role-1",
				},
				{
					Namespace:          "kube-system",
					ServiceAccountName: "aws-ebs-csi-driver",
					RoleARN:            "role-2",
				},
				{
					Namespace:          "karpenter",
					ServiceAccountName: "karpenter",
					RoleARN:            "karpenter-role",
				},
			},
			mockCalls: func(m piaMocks) {
				mockListPodIdentityAssociations(m.eks, true, []listPodIdentityInput{
					{
						namespace:      "kube-system",
						serviceAccount: "vpc-cni",
					},
					{
						namespace:      "kube-system",
						serviceAccount: "aws-ebs-csi-driver",
					},
					{
						namespace:      "karpenter",
						serviceAccount: "karpenter",
					},
				})
				mockDescribePodIdentityAssociation(m.eks, "role-1", "role-2", "role-3")
				for _, serviceAccount := range []string{"vpc-cni", "aws-ebs-csi-driver"} {
					m.stackDescriber.On("DescribeStack", mock.Anything, &manager.Stack{
						StackName: aws.String(fmt.Sprintf("eksctl-test-addon--podidentityrole-%s", serviceAccount)),
					}).Return(nil, &smithy.OperationError{
						Err: fmt.Errorf("ValidationError"),
					}).Once()
				}
				m.stackDescriber.On("DescribeStack", mock.Anything, &manager.Stack{
					StackName: aws.String("eksctl-test-addon--podidentityrole-karpenter"),
				}).Return(&manager.Stack{}, nil).Once()
			},
			expectedErr: "cannot change podIdentityAssociation.roleARN since the role was created by eksctl",
		}),

		Entry("addon contains pod identity created with a pre-existing roleARN and is being updated", updateEntry{
			podIdentityAssociations: []api.PodIdentityAssociation{
				{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
					RoleARN:            "vpc-cni-role-2",
				},
			},
			mockCalls: func(m piaMocks) {
				mockListPodIdentityAssociations(m.eks, true, []listPodIdentityInput{
					{
						namespace:      "kube-system",
						serviceAccount: "vpc-cni",
					},
				})
				mockDescribePodIdentityAssociation(m.eks, "vpc-cni-role")
				m.stackDescriber.On("DescribeStack", mock.Anything, &manager.Stack{
					StackName: aws.String("eksctl-test-addon--podidentityrole-vpc-cni"),
				}).Return(&manager.Stack{}, nil).Once()
			},
			expectedAddonPodIdentityAssociations: []ekstypes.AddonPodIdentityAssociations{
				{
					RoleArn:        aws.String("vpc-cni-role-2"),
					ServiceAccount: aws.String("vpc-cni"),
				},
			},
			expectedErr: "cannot change podIdentityAssociation.roleARN since the role was created by eksctl",
		}),

		Entry("addon contains pod identity created with a pre-existing roleARN but it is no longer set", updateEntry{
			podIdentityAssociations: []api.PodIdentityAssociation{
				{
					Namespace:          "kube-system",
					ServiceAccountName: "vpc-cni",
				},
			},
			mockCalls: func(m piaMocks) {
				mockListPodIdentityAssociations(m.eks, true, []listPodIdentityInput{
					{
						namespace:      "kube-system",
						serviceAccount: "vpc-cni",
					},
				})
				mockDescribePodIdentityAssociation(m.eks, "vpc-cni-role")
				m.stackDescriber.On("DescribeStack", mock.Anything, &manager.Stack{
					StackName: aws.String("eksctl-test-addon--podidentityrole-vpc-cni"),
				}).Return(nil, &smithy.OperationError{
					Err: errors.New("ValidationError"),
				})
			},
			expectedErr: "podIdentityAssociation.roleARN is required since the role was not created by eksctl",
		}),
	)
})
