package awsfederatedaccountaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"

	controllerutils "github.com/ravitri/aws-account-operator/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"

	awsv1alpha1 "github.com/ravitri/aws-account-operator/api/v1alpha1"
	"github.com/ravitri/aws-account-operator/config"
	"github.com/ravitri/aws-account-operator/pkg/awsclient"
)

const (
	controllerName = "awsfederatedaccountaccess"
)

// Custom errors

// ErrFederatedAccessRoleNotFound indicates the role requested by AWSFederatedAccountAccess Cr was not found as a AWSFederatedRole Cr
var ErrFederatedAccessRoleNotFound = errors.New("FederatedAccessRoleNotFound")

// ErrFederatedAccessRoleFailedCreate indicates that the AWSFederatedRole requested failed to be created in the account requested by the AWSFederatedAccountAccess CR
var ErrFederatedAccessRoleFailedCreate = errors.New("FederatedAccessRoleFailedCreate")

var log = logf.Log.WithName("controller_awsfederatedaccountaccess")

// AWSFederatedAccountAccessReconciler reconciles a AWSFederatedAccountAccess object
type AWSFederatedAccountAccessReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	awsClientBuilder awsclient.IBuilder
}

//+kubebuilder:rbac:groups=aws.managed.openshift.io,resources=awsfederatedaccountaccesses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=aws.managed.openshift.io,resources=awsfederatedaccountaccesses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=aws.managed.openshift.io,resources=awsfederatedaccountaccesses/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the AWSFederatedAccountAccess object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *AWSFederatedAccountAccessReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	reqLogger := log.WithValues("Controller", controllerName, "Request.Namespace", request.Namespace, "Request.Name", request.Name)

	// Fetch the AWSFederatedAccountAccess instance
	currentFAA := &awsv1alpha1.AWSFederatedAccountAccess{}
	err := r.Client.Get(context.TODO(), request.NamespacedName, currentFAA)
	if err != nil {
		if k8serr.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	requestedRole := &awsv1alpha1.AWSFederatedRole{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: currentFAA.Spec.AWSFederatedRole.Name, Namespace: currentFAA.Spec.AWSFederatedRole.Namespace}, requestedRole)
	if err != nil {
		if k8serr.IsNotFound(err) {
			SetStatuswithCondition(currentFAA, "Requested role does not exist", awsv1alpha1.AWSFederatedAccountFailed, awsv1alpha1.AWSFederatedAccountStateFailed)
			reqLogger.Error(ErrFederatedAccessRoleNotFound, fmt.Sprintf("Requested role %s not found", currentFAA.Spec.AWSFederatedRole.Name))

			err := r.Client.Status().Update(context.TODO(), currentFAA)
			if err != nil {
				reqLogger.Error(err, fmt.Sprintf("Status update for %s failed", currentFAA.Name))
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil

		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Add finalizer to the CR in case it's not present (e.g. old accounts)
	if !controllerutils.Contains(currentFAA.GetFinalizers(), controllerutils.Finalizer) {

		err := r.addFinalizer(reqLogger, currentFAA)
		if err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	if currentFAA.DeletionTimestamp != nil {

		if controllerutils.Contains(currentFAA.GetFinalizers(), controllerutils.Finalizer) {

			reqLogger.Info("Cleaning up FederatedAccountAccess Roles")
			err = r.cleanFederatedRoles(reqLogger, currentFAA, requestedRole)
			if err != nil {
				return reconcile.Result{}, err
			}

			reqLogger.Info("Removing Finalizer")
			err = r.removeFinalizer(reqLogger, currentFAA, controllerutils.Finalizer)
			if err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	// If the state is ready or failed don't do anything
	if currentFAA.Status.State == awsv1alpha1.AWSFederatedAccountStateReady || currentFAA.Status.State == awsv1alpha1.AWSFederatedAccountStateFailed {
		return reconcile.Result{}, nil
	}

	// Check if the FAA has the uid label
	if !hasLabel(currentFAA, awsv1alpha1.UIDLabel) {
		// Generate a new UID
		uid := controllerutils.GenerateShortUID()

		reqLogger.Info(fmt.Sprintf("Adding UID %s to AccountAccess %s", uid, currentFAA.Name))
		newLabel := map[string]string{"uid": uid}

		// Join the new UID label with any current labels
		if currentFAA.Labels != nil {
			currentFAA.Labels = controllerutils.JoinLabelMaps(currentFAA.Labels, newLabel)
		} else {
			currentFAA.Labels = newLabel
		}

		// Update the CR with new labels
		err = r.Client.Update(context.TODO(), currentFAA)
		if err != nil {
			reqLogger.Error(err, fmt.Sprintf("Lable update for %s failed", currentFAA.Name))
			return reconcile.Result{}, err
		}

	}

	uidLabel, ok := currentFAA.Labels[awsv1alpha1.UIDLabel]
	if !ok {
		return reconcile.Result{}, err
	}

	// Get aws client
	awsRegion := config.GetDefaultRegion()
	awsClient, err := r.awsClientBuilder.GetClient(controllerName, r.Client, awsclient.NewAwsClientInput{
		SecretName: currentFAA.Spec.AWSCustomerCredentialSecret.Name,
		NameSpace:  currentFAA.Spec.AWSCustomerCredentialSecret.Namespace,
		AwsRegion:  awsRegion,
	})
	if err != nil {
		reqLogger.Error(err, "Unable to create aws client for region ")
		return reconcile.Result{}, err
	}

	// Get account number of cluster account
	gciOut, err := awsClient.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		SetStatuswithCondition(currentFAA, "Failed to get account ID information", awsv1alpha1.AWSFederatedAccountFailed, awsv1alpha1.AWSFederatedAccountStateFailed)
		controllerutils.LogAwsError(log, fmt.Sprintf("Failed to get account ID information for '%s'", currentFAA.Name), err, err)
		err := r.Client.Status().Update(context.TODO(), currentFAA)
		if err != nil {
			reqLogger.Error(err, fmt.Sprintf("Status update for %s failed", currentFAA.Name))
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, err
	}

	accountID := *gciOut.Account // Add requested aws managed policies to the role

	if !hasLabel(currentFAA, awsv1alpha1.AccountIDLabel) {

		reqLogger.Info(fmt.Sprintf("Adding awsAccountID %s to AccountAccess %s", accountID, currentFAA.Name))
		newLabel := map[string]string{"awsAccountID": accountID}

		// Join the new UID label with any current labels
		if currentFAA.Labels != nil {
			currentFAA.Labels = controllerutils.JoinLabelMaps(currentFAA.Labels, newLabel)
		} else {
			currentFAA.Labels = newLabel
		}

		// Update the CR with new labels
		err = r.Client.Update(context.TODO(), currentFAA)
		if err != nil {
			reqLogger.Error(err, fmt.Sprintf("Label update for %s failed", currentFAA.Name))
			return reconcile.Result{}, err
		}
	}

	// Here create the custom policy in the cluster account
	err = r.createOrUpdateIAMPolicy(awsClient, *requestedRole, *currentFAA)
	if err != nil {
		// if we were unable to create the policy fail this CR.
		SetStatuswithCondition(currentFAA, "Failed to create custom policy", awsv1alpha1.AWSFederatedAccountFailed, awsv1alpha1.AWSFederatedAccountStateFailed)
		reqLogger.Error(err, fmt.Sprintf("Unable to create policy requested by '%s'", currentFAA.Name))

		err := r.Client.Status().Update(context.TODO(), currentFAA)
		if err != nil {
			reqLogger.Error(err, fmt.Sprintf("Status update for %s failed", currentFAA.Name))
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	// Create role and apply custom policies and awsmanagedpolicies
	role, err := r.createOrUpdateIAMRole(awsClient, *requestedRole, *currentFAA, reqLogger)

	if err != nil {
		SetStatuswithCondition(currentFAA, "Failed to create role", awsv1alpha1.AWSFederatedAccountFailed, awsv1alpha1.AWSFederatedAccountStateFailed)
		reqLogger.Error(ErrFederatedAccessRoleFailedCreate, fmt.Sprintf("Unable to create role requested by '%s'", currentFAA.Name), "AWS ERROR: ", err)

		err := r.Client.Status().Update(context.TODO(), currentFAA)
		if err != nil {
			reqLogger.Error(err, fmt.Sprintf("Status update for %s failed", currentFAA.Name))
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	currentFAA.Status.ConsoleURL = fmt.Sprintf("https://signin.aws.amazon.com/switchrole?account=%s&roleName=%s", accountID, *role.RoleName)

	awsManagedPolicyNames := []string{}
	// Add all aws managed policy names to a array
	awsManagedPolicyNames = append(awsManagedPolicyNames, requestedRole.Spec.AWSManagedPolicies...)
	// Get policy arns for managed policies
	policyArns := createPolicyArns(accountID, awsManagedPolicyNames, true)
	// Get custom policy arns
	customPolicy := []string{requestedRole.Spec.AWSCustomPolicy.Name + "-" + uidLabel}
	customerPolArns := createPolicyArns(accountID, customPolicy, false)
	policyArns = append(policyArns, customerPolArns[0])

	// Attach the requested policy to the newly created role
	err = r.attachIAMPolices(awsClient, currentFAA.Spec.AWSFederatedRole.Name+"-"+uidLabel, policyArns)
	if err != nil {
		//TODO() role should be deleted here so that we leave nothing behind.

		SetStatuswithCondition(currentFAA, "Failed to attach policies to role", awsv1alpha1.AWSFederatedAccountFailed, awsv1alpha1.AWSFederatedAccountStateFailed)
		reqLogger.Error(err, fmt.Sprintf("Failed to attach policies to role requested by '%s'", currentFAA.Name))
		err := r.Client.Status().Update(context.TODO(), currentFAA)
		if err != nil {
			reqLogger.Error(err, fmt.Sprintf("Status update for %s failed", currentFAA.Name))
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}
	// Mark AWSFederatedAccountAccess CR as Ready.
	SetStatuswithCondition(currentFAA, "Account Access Ready", awsv1alpha1.AWSFederatedAccountReady, awsv1alpha1.AWSFederatedAccountStateReady)
	reqLogger.Info(fmt.Sprintf("Successfully applied %s", currentFAA.Name))
	err = r.Client.Status().Update(context.TODO(), currentFAA)
	if err != nil {
		reqLogger.Error(err, fmt.Sprintf("Status update for %s failed", currentFAA.Name))
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// createIAMPolicy creates the IAM policys in AWSFederatedRole inside of our cluster account
func (r *AWSFederatedAccountAccessReconciler) createIAMPolicy(awsClient awsclient.Client, afr awsv1alpha1.AWSFederatedRole, afaa awsv1alpha1.AWSFederatedAccountAccess) (*iam.Policy, error) {
	// Same struct from the afr.Spec.AWSCustomPolicy.Statements , but with json tags as capitals due to requirements for the policydoc
	type awsStatement struct {
		Effect    string                 `json:"Effect"`
		Action    []string               `json:"Action"`
		Resource  []string               `json:"Resource,omitempty"`
		Condition *awsv1alpha1.Condition `json:"Condition,omitempty"`
		Principal *awsv1alpha1.Principal `json:"Principal,omitempty"`
	}

	statements := []awsStatement{}

	for _, sm := range afr.Spec.AWSCustomPolicy.Statements {
		var a awsStatement = awsStatement(sm)
		statements = append(statements, a)
	}

	// Create an aws policydoc formated struct
	policyDoc := struct {
		Version   string
		Statement []awsStatement
	}{
		Version:   "2012-10-17",
		Statement: statements,
	}

	// Marshal policydoc to json
	jsonPolicyDoc, err := json.Marshal(&policyDoc)
	if err != nil {
		return &iam.Policy{}, fmt.Errorf("Error marshalling jsonPolicy doc : Error %s", err.Error())
	}

	var policyName string
	// Try and build policy name
	if uidLabel, ok := afaa.Labels["uid"]; ok {
		policyName = afr.Spec.AWSCustomPolicy.Name + "-" + uidLabel
	} else {
		// Just in case the UID somehow doesn't exist
		return nil, errors.New("Failed to get UID label")
	}

	output, err := awsClient.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(policyName),
		Description:    aws.String(afr.Spec.AWSCustomPolicy.Description),
		PolicyDocument: aws.String(string(jsonPolicyDoc)),
	})
	if err != nil {
		return nil, err
	}

	return output.Policy, nil
}

func (r *AWSFederatedAccountAccessReconciler) createIAMRole(awsClient awsclient.Client, afr awsv1alpha1.AWSFederatedRole, afaa awsv1alpha1.AWSFederatedAccountAccess) (*iam.Role, error) {
	type awsStatement struct {
		Effect    string                 `json:"Effect"`
		Action    []string               `json:"Action"`
		Resource  []string               `json:"Resource,omitempty"`
		Principal *awsv1alpha1.Principal `json:"Principal,omitempty"`
	}

	assumeRolePolicyDoc := struct {
		Version   string
		Statement []awsStatement
	}{
		Version: "2012-10-17",
		Statement: []awsStatement{{
			Effect: "Allow",
			Action: []string{"sts:AssumeRole"},
			Principal: &awsv1alpha1.Principal{
				AWS: []string{afaa.Spec.ExternalCustomerAWSIAMARN},
			},
		}},
	}

	// Marshal assumeRolePolicyDoc to json
	jsonAssumeRolePolicyDoc, err := json.Marshal(&assumeRolePolicyDoc)
	if err != nil {
		return nil, err
	}

	var roleName string
	// Try and build role name
	if uidLabel, ok := afaa.Labels["uid"]; ok {
		roleName = afr.Name + "-" + uidLabel
	} else {
		// Just in case the UID somehow doesn't exist
		return nil, errors.New("Failed to get UID label")
	}

	createRoleOutput, err := awsClient.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		Description:              aws.String(afr.Spec.RoleDescription),
		AssumeRolePolicyDocument: aws.String(string(jsonAssumeRolePolicyDoc)),
	})
	if err != nil {
		return nil, err
	}

	return createRoleOutput.Role, nil
}

func (r *AWSFederatedAccountAccessReconciler) createOrUpdateIAMPolicy(awsClient awsclient.Client, afr awsv1alpha1.AWSFederatedRole, afaa awsv1alpha1.AWSFederatedAccountAccess) error {

	uidLabel, ok := afaa.Labels["uid"]
	if !ok {
		return errors.New("Unable to get UID label")
	}

	gciOut, err := awsClient.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return err
	}

	customPolArns := createPolicyArns(*gciOut.Account, []string{afr.Spec.AWSCustomPolicy.Name + "-" + uidLabel}, false)

	_, err = r.createIAMPolicy(awsClient, afr, afaa)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "EntityAlreadyExists" {
				_, err = awsClient.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(customPolArns[0])})
				if err != nil {
					return err
				}
				_, err = r.createIAMPolicy(awsClient, afr, afaa)
				if err != nil {
					return err
				}

			}
		}
	}

	return nil
}

func (r *AWSFederatedAccountAccessReconciler) createOrUpdateIAMRole(awsClient awsclient.Client, afr awsv1alpha1.AWSFederatedRole, afaa awsv1alpha1.AWSFederatedAccountAccess, reqLogger logr.Logger) (*iam.Role, error) {

	uidLabel, ok := afaa.Labels["uid"]
	if !ok {
		return nil, errors.New("Unable to get UID label")
	}

	roleName := afaa.Spec.AWSFederatedRole.Name + "-" + uidLabel

	role, err := r.createIAMRole(awsClient, afr, afaa)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "EntityAlreadyExists":
				_, err := awsClient.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)})

				if err != nil {
					return nil, err
				}

				role, err := r.createIAMRole(awsClient, afr, afaa)

				if err != nil {
					return nil, err
				}

				return role, nil
			default:
				// Handle unexpected AWS API errors
				controllerutils.LogAwsError(reqLogger, "createOrUpdateIAMRole: Unexpected AWS Error creating IAM Role", nil, err)
				return nil, err
			}
		}
		// Return all other (non-AWS) errors
		return nil, err
	}

	return role, nil
}

func (r *AWSFederatedAccountAccessReconciler) attachIAMPolices(awsClient awsclient.Client, roleName string, policyArns []string) error {
	for _, pol := range policyArns {
		_, err := awsClient.AttachRolePolicy(&iam.AttachRolePolicyInput{
			PolicyArn: aws.String(pol),
			RoleName:  aws.String(roleName),
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// Pass in the account id of the account where you the policies live.
func createPolicyArns(accountID string, policyNames []string, awsManaged bool) []string {
	awsPolicyArnPrefix := ""

	if awsManaged {
		awsPolicyArnPrefix = "arn:aws:iam::aws:policy/"
	} else {
		awsPolicyArnPrefix = fmt.Sprintf("arn:aws:iam::%s:policy/", accountID)
	}
	policyArns := []string{}
	for _, policy := range policyNames {
		policyArns = append(policyArns, fmt.Sprintf("%s%s", awsPolicyArnPrefix, policy))
	}
	return policyArns
}

// SetStatuswithCondition sets the status of an account
func SetStatuswithCondition(afaa *awsv1alpha1.AWSFederatedAccountAccess, message string, ctype awsv1alpha1.AWSFederatedAccountAccessConditionType, state awsv1alpha1.AWSFederatedAccountAccessState) {
	afaa.Status.Conditions = controllerutils.SetAWSFederatedAccountAccessCondition(
		afaa.Status.Conditions,
		ctype,
		corev1.ConditionTrue,
		string(state),
		message,
		controllerutils.UpdateConditionNever)
	afaa.Status.State = state
}

func (r *AWSFederatedAccountAccessReconciler) addFinalizer(reqLogger logr.Logger, awsFederatedAccountAccess *awsv1alpha1.AWSFederatedAccountAccess) error {
	reqLogger.Info("Adding Finalizer for the AccountClaim")
	awsFederatedAccountAccess.SetFinalizers(append(awsFederatedAccountAccess.GetFinalizers(), controllerutils.Finalizer))

	// Update CR
	err := r.Client.Update(context.TODO(), awsFederatedAccountAccess)
	if err != nil {
		reqLogger.Error(err, "Failed to update AccountClaim with finalizer")
		return err
	}
	return nil
}

func (r *AWSFederatedAccountAccessReconciler) removeFinalizer(reqLogger logr.Logger, AWSFederatedAccountAccess *awsv1alpha1.AWSFederatedAccountAccess, finalizerName string) error {
	reqLogger.Info("Removing Finalizer for the AWSFederatedAccountAccess")
	AWSFederatedAccountAccess.SetFinalizers(controllerutils.Remove(AWSFederatedAccountAccess.GetFinalizers(), finalizerName))

	// Update CR
	err := r.Client.Update(context.TODO(), AWSFederatedAccountAccess)
	if err != nil {
		reqLogger.Error(err, "Failed to remove AWSFederatedAccountAccess finalizer")
		return err
	}
	return nil
}

func (r *AWSFederatedAccountAccessReconciler) cleanFederatedRoles(reqLogger logr.Logger, currentFAA *awsv1alpha1.AWSFederatedAccountAccess, federatedRoleCR *awsv1alpha1.AWSFederatedRole) error {

	// Get the UID
	uidLabel, ok := currentFAA.Labels[awsv1alpha1.UIDLabel]
	if !ok {

		if currentFAA.Status.State != awsv1alpha1.AWSFederatedAccountStateReady {
			log.Info("UID Label missing with CR not ready, removing finalizer")
			return nil
		}
		return errors.New("Unable to get UID label")

	}

	// Get the AWS Account ID
	accountIDLabel, ok := currentFAA.Labels[awsv1alpha1.AccountIDLabel]
	if !ok {
		if currentFAA.Status.State != awsv1alpha1.AWSFederatedAccountStateReady {
			log.Info("AWS Account ID Label missing with CR not ready, removing finalizer")
			return nil
		}
		return errors.New("Unable to get AWS Account ID label")
	}

	roleName := currentFAA.Spec.AWSFederatedRole.Name + "-" + uidLabel

	// Build AWS client from root secret
	awsRegion := config.GetDefaultRegion()
	rootAwsClient, err := r.awsClientBuilder.GetClient(controllerName, r.Client, awsclient.NewAwsClientInput{
		SecretName: controllerutils.AwsSecretName,
		NameSpace:  awsv1alpha1.AccountCrNamespace,
		AwsRegion:  awsRegion,
	})
	if err != nil {
		reqLogger.Error(err, "Unable to create root aws client for region ")
		return err
	}

	assumeRoleOutput, err := rootAwsClient.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(fmt.Sprintf("arn:aws:iam::%s:role/OrganizationAccountAccessRole", accountIDLabel)),
		RoleSessionName: aws.String("FederatedRoleCleanup"),
	})
	if err != nil {
		reqLogger.Info("Unable to assume role OrganizationAccountAccessRole, trying BYOCAdminAccess")

		// Attempt to assume the BYOCAdminAccess role if OrganizationAccountAccess didn't work
		assumeRoleOutput, err = rootAwsClient.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(fmt.Sprintf("arn:aws:iam::%s:role/BYOCAdminAccess-%s", accountIDLabel, uidLabel)),
			RoleSessionName: aws.String("FederatedRoleCleanup"),
		})
		if err != nil {
			reqLogger.Error(err, "Unable to assume role BYOCAdminAccess Role")
			return err
		}

	}

	awsClient, err := r.awsClientBuilder.GetClient(controllerName, r.Client, awsclient.NewAwsClientInput{
		AwsCredsSecretIDKey:     *assumeRoleOutput.Credentials.AccessKeyId,
		AwsCredsSecretAccessKey: *assumeRoleOutput.Credentials.SecretAccessKey,
		AwsToken:                *assumeRoleOutput.Credentials.SessionToken,
		AwsRegion:               awsRegion,
	})
	if err != nil {
		reqLogger.Error(err, "Unable to create aws client for target linked account in region ")
		return err
	}

	var nextMarker *string

	// Paginate through attached policies and attempt to remove them
	reqLogger.Info("Detaching Policies")
	for {
		attachedPolicyOutput, err := awsClient.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{RoleName: aws.String(roleName), Marker: nextMarker})
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case "NoSuchEntity":
					// Delete any custom policies made
					err = r.deleteNonAttachedCustomPolicy(reqLogger, awsClient, currentFAA, federatedRoleCR)
					if err != nil {
						return err
					}
					return nil
				default:
					reqLogger.Error(
						aerr,
						fmt.Sprint(aerr.Error()),
					)
					reqLogger.Error(err, fmt.Sprintf("%v", err))
					return err
				}
			} else {
				reqLogger.Error(err, "NOther error while trying to list policies")
				return err
			}
		}
		for _, attachedPolicy := range attachedPolicyOutput.AttachedPolicies {
			_, err = awsClient.DetachRolePolicy(&iam.DetachRolePolicyInput{RoleName: aws.String(roleName), PolicyArn: attachedPolicy.PolicyArn})
			if err != nil {
				if aerr, ok := err.(awserr.Error); ok {
					switch aerr.Code() {
					default:
						reqLogger.Error(
							aerr,
							fmt.Sprint(aerr.Error()),
						)
						reqLogger.Error(err, fmt.Sprintf("%v", err))
						return err
					}
				} else {
					reqLogger.Error(err, "NOther error while trying to detach policies")
					return err
				}
			}

			err = checkAndDeletePolicy(reqLogger, awsClient, uidLabel, federatedRoleCR.Spec.AWSCustomPolicy.Name, attachedPolicy.PolicyName, attachedPolicy.PolicyArn)
			if err != nil {
				return err
			}
		}

		if *attachedPolicyOutput.IsTruncated {
			nextMarker = attachedPolicyOutput.Marker
		} else {
			break
		}
	}

	// Delete the role
	reqLogger.Info("Deleting Role")
	_, err = awsClient.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			default:
				reqLogger.Error(aerr, fmt.Sprint(aerr.Error()))
				return err
			}
		} else {
			reqLogger.Error(err, "Other error while trying to detach policies")
			return err
		}
	}

	return nil
}

func (r *AWSFederatedAccountAccessReconciler) deleteNonAttachedCustomPolicy(reqLogger logr.Logger, awsClient awsclient.Client, currentFAA *awsv1alpha1.AWSFederatedAccountAccess, federatedRoleCR *awsv1alpha1.AWSFederatedRole) error {

	// Get the UID
	uidLabel, ok := currentFAA.Labels[awsv1alpha1.UIDLabel]
	if !ok {
		return errors.New("Unable to get UID label")
	}

	var policyMarker *string
	// Paginate through custom policies
	for {
		policyListOutput, err := awsClient.ListPolicies(&iam.ListPoliciesInput{Scope: aws.String("Local"), Marker: policyMarker})
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				default:
					reqLogger.Error(aerr, fmt.Sprint(aerr.Error()))
					return err
				}
			}
			return err
		}

		for _, policy := range policyListOutput.Policies {
			err = checkAndDeletePolicy(reqLogger, awsClient, uidLabel, federatedRoleCR.Spec.AWSCustomPolicy.Name, policy.PolicyName, policy.Arn)
			if err != nil {
				return err
			}
		}

		if *policyListOutput.IsTruncated {
			policyMarker = policyListOutput.Marker
		} else {
			break
		}
	}

	return nil
}

func hasLabel(awsFederatedAccountAccess *awsv1alpha1.AWSFederatedAccountAccess, labelKey string) bool {

	// Check if the given key exists as a label
	if _, ok := awsFederatedAccountAccess.Labels[labelKey]; ok {
		return true
	}
	return false
}

func checkAndDeletePolicy(reqLogger logr.Logger, awsClient awsclient.Client, uidLabel string, crPolicyName string, policyName *string, policyArn *string) error {

	var awsCustomPolicyname string
	awsCustomPolicyname = getPolicyNameWithUID(awsCustomPolicyname, crPolicyName, uidLabel)

	if *policyName == awsCustomPolicyname {
		_, err := awsClient.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: policyArn})
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				default:
					reqLogger.Error(
						aerr,
						fmt.Sprint(aerr.Error()),
					)
					reqLogger.Error(err, fmt.Sprintf("%v", err))
					return err
				}
			} else {
				reqLogger.Error(err, "Other error while trying to detach policies")
				return err
			}
		}
	}
	return nil
}

func getPolicyNameWithUID(awsCustomPolicyname string, crPolicyName string, uidLabel string) string {
	if !strings.HasSuffix(crPolicyName, "-"+uidLabel) {
		crPolicyName = crPolicyName + "-" + uidLabel
	}
	return crPolicyName
}

// SetupWithManager sets up the controller with the Manager.
func (r *AWSFederatedAccountAccessReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.awsClientBuilder = &awsclient.Builder{}
	maxReconciles, err := controllerutils.GetControllerMaxReconciles(controllerName)
	if err != nil {
		log.Error(err, "missing max reconciles for controller", "controller", controllerName)
	}

	rwm := controllerutils.NewReconcilerWithMetrics(r, controllerName)
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsv1alpha1.AWSFederatedAccountAccess{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxReconciles,
		}).Complete(rwm)
}
