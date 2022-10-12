resource "aws_s3_bucket" "this" {
  bucket_prefix = "${var.name}-"
  force_destroy = true
  tags = {
    Name = var.name
  }
}

resource "aws_s3_bucket_acl" "this" {
  bucket = aws_s3_bucket.this.id
  acl    = "private"
}

resource "aws_s3_bucket_versioning" "this" {
  bucket = aws_s3_bucket.this.id
  versioning_configuration {
    status = "Disabled"
  }
}
