output "cert_manager_metadata" {
  value       = try(one(module.cert_manager.metadata), null)
  description = "Block status of the deployed release"
}

output "cert_manager_issuer_metadata" {
  value       = try(one(module.cert_manager_issuer.metadata), null)
  description = "Block status of the deployed release"
}

output "service_account_role_arn" {
  value       = module.cert_manager.service_account_role_arn
  description = "ARN of the IAM role created for the cert-manager ServiceAccount when `letsencrypt_enabled` is `true`. Verify it matches the `eks.amazonaws.com/role-arn` annotation on the ServiceAccount."
}
