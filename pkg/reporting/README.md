# reporting

The autoscaler-agent reports multiple types of data (billing data, scaling events) in multiple ways
(HTTP, S3, Azure Blob), so `reporting` is the abstraction allowing us to deduplicate code between
them.
