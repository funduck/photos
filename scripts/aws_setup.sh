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
