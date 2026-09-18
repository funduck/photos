# Just a collection of steps required to setup AWS S3 bucket and IAM user for rclone photo archive
# Run manually one by one

aws configure

aws s3api create-bucket \
  --bucket $AWS_BUCKET \
  --region $AWS_REGION \
  --create-bucket-configuration LocationConstraint=$AWS_REGION

aws s3api put-public-access-block \
  --bucket $AWS_BUCKET \
  --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true

aws s3api put-bucket-encryption \
  --bucket $AWS_BUCKET \
  --server-side-encryption-configuration '{
    "Rules": [{"ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "AES256"}}]
  }'

aws iam create-policy \
  --policy-name rclone-photo-archive-policy \
  --policy-document file://rclone-archive-policy.json

aws iam create-user --user-name rclone-photo-archive

aws iam attach-user-policy \
  --user-name rclone-photo-archive \
  --policy-arn arn:aws:iam::$AWS_ACCOUNT_ID:policy/rclone-photo-archive-policy

aws iam create-access-key --user-name rclone-photo-archive

aws budgets create-budget \
  --account-id $AWS_ACCOUNT_ID \
  --budget '{
    "BudgetName": "photo-archive-guard",
    "BudgetLimit": {"Amount": "1", "Unit": "USD"},
    "TimeUnit": "MONTHLY",
    "BudgetType": "COST"
  }' \
  --notifications-with-subscribers '[{
    "Notification": {
      "NotificationType": "ACTUAL",
      "ComparisonOperator": "GREATER_THAN",
      "Threshold": 100
    },
    "Subscribers": [{"SubscriptionType": "EMAIL", "Address": "'"$EMAIL"'"}]
  }]'

# --- Temporary delete permission (for scripts/delete_files_s3.sh) ---
# The rclone user is normally add-only. Grant DeleteObject as an inline policy,
# run the deletes, then remove the policy again right away.

# Versioned buckets turn deletes into delete markers, so the objects (and their
# cost) stay. Expect "Enabled" to mean you need a different approach.
aws s3api get-bucket-versioning --bucket $AWS_BUCKET

aws iam put-user-policy \
  --user-name rclone-photo-archive \
  --policy-name rclone-temp-delete \
  --policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Sid": "TempDelete",
      "Effect": "Allow",
      "Action": ["s3:DeleteObject"],
      "Resource": "arn:aws:s3:::'"$AWS_BUCKET"'/*"
    }]
  }'

# IAM changes can take a few seconds to apply. Confirm the policy is there:
aws iam get-user-policy --user-name rclone-photo-archive --policy-name rclone-temp-delete

# ... run scripts/delete_files_s3.sh to_delete.txt --execute ...
# (or, to drop the old pre-root-layout prefix: rclone purge $RCLONE_REMOTE:$AWS_BUCKET/Photos)

# Revoke, then confirm the list is empty:
aws iam delete-user-policy --user-name rclone-photo-archive --policy-name rclone-temp-delete
aws iam list-user-policies --user-name rclone-photo-archive
