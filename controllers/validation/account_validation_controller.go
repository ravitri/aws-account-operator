package validation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/organizations"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	awsv1alpha1 "github.com/ravitri/aws-account-operator/api/v1alpha1"
	"github.com/ravitri/aws-account-operator/config"
	"github.com/ravitri/aws-account-operator/controllers/account"
	"github.com/ravitri/aws-account-operator/pkg/awsclient"
	"github.com/ravitri/aws-account-operator/pkg/utils"
)

var log = logf.Log.WithName("controller_accountvalidation")

var accountMoveEnabled = false
var accountTagEnabled = false

const (
	controllerName = "accountvalidation"
	moveWaitTime   = 5 * time.Minute
	ownerKey       = "owner"
)

type AccountValidationReconciler struct {
	Client           client.Client
	Scheme           *runtime.Scheme
	awsClientBuilder awsclient.IBuilder
}

type ValidationError int64

const (
	InvalidAccount ValidationError = iota
	AccountMoveFailed
	MissingTag
	IncorrectOwnerTag
	AccountTagFailed
	MissingAWSAccount
)

type AccountValidationError struct {
	Type ValidationError
	Err  error
}

func NewAccountValidationReconciler(client client.Client, scheme *runtime.Scheme, awsClientBuilder awsclient.IBuilder) *AccountValidationReconciler {
	return &AccountValidationReconciler{
		Client:           client,
		Scheme:           scheme,
		awsClientBuilder: awsClientBuilder,
	}
}

func (ave *AccountValidationError) Error() string {
	return ave.Err.Error()
}

// Retrieve all parents of the given awsId until the predicate returns true.
func ParentsTillPredicate(awsId string, client awsclient.Client, p func(s string) bool, parents *[]string) error {
	listParentsInput := organizations.ListParentsInput{
		ChildId: aws.String(awsId),
	}
	listParentsOutput, err := client.ListParents(&listParentsInput)
	if err != nil {
		return err
	}
	if len(listParentsOutput.Parents) == 0 {
		log.Info("Exhausted search looking for root OU - root OU and account OU likely in separate subtrees.", "path", parents)
		return nil
	} else if len(listParentsOutput.Parents) > 1 {
		log.Info("More than 1 parent returned for an ID - unexpected.", "awsId", awsId)
		return errors.New("More than 1 parents found for Id " + awsId)
	} else {
		id := *listParentsOutput.Parents[0].Id
		*parents = append(*parents, id)
		if p(id) {
			return nil
		}
		return ParentsTillPredicate(id, client, p, parents)
	}
}

// Verify if the account is already in the root OU
// The predicate indicates if the parent considered the desired root was found.
func IsAccountInPoolOU(account awsv1alpha1.Account, client awsclient.Client, isPoolOU func(s string) bool) bool {
	if account.Spec.AwsAccountID == "" {
		return false
	}
	parentList := []string{}
	err := ParentsTillPredicate(account.Spec.AwsAccountID, client, isPoolOU, &parentList)
	if err != nil {
		return false
	}
	if len(parentList) == 1 {
		return true
	}
	return false
}

func MoveAccount(awsAccountId string, client awsclient.Client, targetOU string, moveAccount bool) error {
	listParentsInput := organizations.ListParentsInput{
		ChildId: aws.String(awsAccountId),
	}
	listParentsOutput, err := client.ListParents(&listParentsInput)
	if err != nil {
		log.Error(err, "Can not find parent for AWS account", "aws-account", awsAccountId)
		return err
	}
	oldOu := listParentsOutput.Parents[0].Id
	if moveAccount {
		log.Info("Moving aws account from old ou to new ou", "aws-account", awsAccountId, "old-ou", *oldOu, "new-ou", targetOU)
		moveAccountInput := organizations.MoveAccountInput{
			AccountId:           aws.String(awsAccountId),
			DestinationParentId: aws.String(targetOU),
			SourceParentId:      oldOu,
		}
		_, err = client.MoveAccount(&moveAccountInput)
		if err != nil {
			log.Error(err, "Could not move aws account to new ou", "aws-account", awsAccountId, "ou", targetOU)
			return err
		}
	} else {
		log.Info("Not moving aws account from old ou to new ou (dry run)", "aws-account", awsAccountId, "old-ou", *oldOu, "new-ou", targetOU)
	}
	return nil
}

func untagAccountOwner(client awsclient.Client, accountId string) error {
	inputTags := &organizations.UntagResourceInput{
		ResourceId: aws.String(accountId),
		TagKeys:    []*string{aws.String("owner")},
	}

	_, err := client.UntagResource(inputTags)
	return err
}

func ValidateAccountTags(client awsclient.Client, accountId *string, shardName string, accountTagEnabled bool) error {
	listTagsForResourceInput := &organizations.ListTagsForResourceInput{
		ResourceId: accountId,
	}

	resp, err := client.ListTagsForResource(listTagsForResourceInput)
	if err != nil {
		return err
	}

	for _, tag := range resp.Tags {
		if ownerKey == *tag.Key {
			if shardName != *tag.Value {
				if accountTagEnabled {
					err := untagAccountOwner(client, *accountId)
					if err != nil {
						log.Error(err, "Unable to remove incorrect owner tag from aws account.", "AWSAccountId", accountId)
						return &AccountValidationError{
							Type: AccountTagFailed,
							Err:  err,
						}
					}

					err = account.TagAccount(client, *accountId, shardName)
					if err != nil {
						log.Error(err, "Unable to tag aws account.", "AWSAccountID", accountId)
						return &AccountValidationError{
							Type: AccountTagFailed,
							Err:  err,
						}
					}

					return nil
				} else {
					return &AccountValidationError{
						Type: IncorrectOwnerTag,
						Err:  fmt.Errorf("Account is not tagged with the correct owner, has %s; want %s", *tag.Value, shardName),
					}
				}
			} else {
				return nil
			}
		}
	}

	if accountTagEnabled {
		err := account.TagAccount(client, *accountId, shardName)
		if err != nil {
			log.Error(err, "Unable to tag aws account.", "AWSAccountID", accountId)
			return &AccountValidationError{
				Type: AccountTagFailed,
				Err:  err,
			}
		}
		return nil
	} else {
		return &AccountValidationError{
			Type: MissingTag,
			Err:  errors.New("Account is not tagged with an owner"),
		}
	}
}

func ValidateAccountOrigin(account awsv1alpha1.Account) error {
	// Perform basic short-circuit checks
	if account.IsBYOC() {
		log.Info("Will not validate a CCS account")
		return &AccountValidationError{
			Type: InvalidAccount,
			Err:  errors.New("Account is a CCS account"),
		}
	}
	if !account.IsOwnedByAccountPool() {
		log.Info("Will not validate account not owned by account pool")
		return &AccountValidationError{
			Type: InvalidAccount,
			Err:  errors.New("Account is not in an account pool"),
		}
	}
	if !account.IsReady() {
		log.Info("Will not validate account not in a ready state")
		return &AccountValidationError{
			Type: InvalidAccount,
			Err:  errors.New("Account is not in a ready state"),
		}
	}
	return nil
}

func ValidateAwsAccountId(account awsv1alpha1.Account) error {
	if account.Spec.AwsAccountID == "" {
		return &AccountValidationError{
			Type: MissingAWSAccount,
			Err:  errors.New("Account has not associated AWS account"),
		}
	}
	return nil
}

func (r *AccountValidationReconciler) ValidateAccountOU(awsClient awsclient.Client, account awsv1alpha1.Account, poolOU string) error {
	// Perform all checks on the account we want.
	inPool := IsAccountInPoolOU(account, awsClient, func(s string) bool {
		return s == poolOU
	})
	if inPool {
		log.Info("Account is already in the root OU.")
	} else {
		log.Info("Account is not in the root OU - it will be moved.")
		err := MoveAccount(account.Spec.AwsAccountID, awsClient, poolOU, accountMoveEnabled)
		if err != nil {
			log.Error(err, "Could not move account")
			return &AccountValidationError{
				Type: AccountMoveFailed,
				Err:  err,
			}
		}
	}
	return nil
}

func (r *AccountValidationReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log.WithValues("Controller", controllerName, "Request.Namespace", request.Namespace, "Request.Name", request.Name)

	// Setup: retrieve account and awsClient
	var account awsv1alpha1.Account
	err := r.Client.Get(context.TODO(), request.NamespacedName, &account)
	if err != nil {
		log.Error(err, "Could not retrieve account to validate", "account-request", request.NamespacedName)
		return utils.DoNotRequeue()
	}

	cm, err := utils.GetOperatorConfigMap(r.Client)
	if err != nil {
		log.Error(err, "Could not retrieve the operator configmap")
		return utils.RequeueAfter(5 * time.Minute)
	}

	enabled, err := strconv.ParseBool(cm.Data["feature.validation_move_account"])
	if err != nil {
		log.Info("Could not retrieve feature flag 'feature.validation_move_account' - account moving is disabled")
	} else {
		accountMoveEnabled = enabled
	}
	log.Info("Is moving accounts enabled?", "enabled", accountMoveEnabled)

	enabled, err = strconv.ParseBool(cm.Data["feature.validation_tag_account"])
	if err != nil {
		log.Info("Could not retrieve feature flag 'feature.validation_tag_account' - account tagging is disabled")
	} else {
		accountTagEnabled = enabled
	}
	log.Info("Is tagging accounts enabled?", "enabled", accountTagEnabled)

	awsClientInput := awsclient.NewAwsClientInput{
		AwsRegion:  config.GetDefaultRegion(),
		SecretName: utils.AwsSecretName,
		NameSpace:  awsv1alpha1.AccountCrNamespace,
	}
	awsClient, err := r.awsClientBuilder.GetClient(controllerName, r.Client, awsClientInput)
	if err != nil {
		log.Error(err, "Could not retrieve AWS client.")
	}

	// Perform any checks we want
	err = ValidateAccountOrigin(account)
	if err != nil {
		// Decide who we will requeue now
		validationError, ok := err.(*AccountValidationError)
		if ok && validationError.Type == InvalidAccount {
			return utils.DoNotRequeue()
		}
		return utils.RequeueWithError(err)
	}

	err = ValidateAwsAccountId(account)
	if err != nil {
		validationError, ok := err.(*AccountValidationError)
		if ok && validationError.Type == MissingAWSAccount {
			return utils.DoNotRequeue()
		}
		return utils.RequeueWithError(err)
	}

	err = r.ValidateAccountOU(awsClient, account, cm.Data["root"])
	if err != nil {
		// Decide who we will requeue now
		validationError, ok := err.(*AccountValidationError)
		if ok && validationError.Type == AccountMoveFailed {
			return utils.RequeueAfter(moveWaitTime)
		}
		return utils.RequeueWithError(err)
	}

	shardName, ok := cm.Data["shard-name"]
	if !ok {
		log.Info("Could not retrieve configuration map value 'shard-name' - account tagging is disabled")
	} else {
		if shardName == "" {
			log.Info("Cluster configuration is missing a shardName value.  Skipping validation for this tag.")
		} else {
			err = ValidateAccountTags(awsClient, aws.String(account.Spec.AwsAccountID), shardName, accountTagEnabled)
			if err != nil {
				validationError, ok := err.(*AccountValidationError)
				if ok && (validationError.Type == MissingTag || validationError.Type == IncorrectOwnerTag) {
					log.Error(validationError, validationError.Err.Error())
				}
				return utils.RequeueWithError(err)
			}
		}
	}
	return utils.DoNotRequeue()
}

// SetupWithManager sets up the controller with the Manager.
func (r *AccountValidationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.awsClientBuilder = &awsclient.Builder{}
	maxReconciles, err := utils.GetControllerMaxReconciles(controllerName)
	if err != nil {
		log.Error(err, "missing max reconciles for controller", "controller", controllerName)
	}

	rwm := utils.NewReconcilerWithMetrics(r, controllerName)
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsv1alpha1.Account{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxReconciles,
		}).Complete(rwm)
}
