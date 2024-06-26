package minio

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

func resourceMinioILMPolicy() *schema.Resource {
	return &schema.Resource{
		CreateContext: minioCreateILMPolicy,
		ReadContext:   minioReadILMPolicy,
		DeleteContext: minioDeleteILMPolicy,
		UpdateContext: minioUpdateILMPolicy,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Description: "`minio_ilm_policy` handles lifecycle settings for a given `minio_s3_bucket`.",
		Schema: map[string]*schema.Schema{
			"bucket": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringLenBetween(0, 63),
			},
			"rule": {
				Type:     schema.TypeList,
				Required: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"id": {
							Type:     schema.TypeString,
							Required: true,
						},
						"expiration": {
							Type:             schema.TypeString,
							Optional:         true,
							Description:      "Value may be duration (5d), date (1970-01-01), or \"DeleteMarker\" to expire delete markers if `noncurrent_version_expiration_days` is used",
							ValidateDiagFunc: validateILMExpiration,
						},

						"transition": {
							Type:     schema.TypeList,
							MaxItems: 1,
							Optional: true,
							Elem: &schema.Resource{
								Schema: map[string]*schema.Schema{
									"days": {
										Type:     schema.TypeString,
										Optional: true,
									},
									"date": {
										Type:     schema.TypeString,
										Optional: true,
									},
									"storage_class": {
										Type:     schema.TypeString,
										Required: true,
									},
								},
							},
						},
						"noncurrent_version_expiration_days": {
							Type:             schema.TypeInt,
							Optional:         true,
							ValidateDiagFunc: validateILMNoncurrentVersionExpiration,
						},
						"noncurrent_version_transition_days": {
							Type:             schema.TypeInt,
							Optional:         true,
							ValidateDiagFunc: validateILMNoncurrentVersionTransition,
						},
						"status": {
							Type:     schema.TypeString,
							Computed: true,
						},
						"filter": {
							Type:     schema.TypeString,
							Optional: true,
						},
						"tags": {
							Type:     schema.TypeMap,
							Optional: true,
						},
					},
				},
			},
		},
	}
}

func validateILMExpiration(v interface{}, p cty.Path) (errors diag.Diagnostics) {
	value := v.(string)
	exp := parseILMExpiration(value)

	if (lifecycle.Expiration{}) == exp {
		return diag.Errorf("expiration must be a duration (5d), date (1970-01-01), or \"DeleteMarker\"")
	}

	return
}

func validateILMNoncurrentVersionExpiration(v interface{}, p cty.Path) (errors diag.Diagnostics) {
	value := v.(int)

	if value < 1 {
		return diag.Errorf("noncurrent_version_expiration_days must be strictly positive")
	}

	return
}

func validateILMNoncurrentVersionTransition(v interface{}, p cty.Path) (errors diag.Diagnostics) {
	value := v.(int)

	if value < 1 {
		return diag.Errorf("noncurrent_version_transition_days must be strictly positive")
	}

	return
}

func minioCreateILMPolicy(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c := meta.(*S3MinioClient).S3Client

	config := lifecycle.NewConfiguration()

	bucket := d.Get("bucket").(string)
	rules := d.Get("rule").([]interface{})
	for _, ruleI := range rules {
		rule := ruleI.(map[string]interface{})

		var filter lifecycle.Filter

		noncurrentVersionExpirationDays := lifecycle.NoncurrentVersionExpiration{NoncurrentDays: lifecycle.ExpirationDays(rule["noncurrent_version_expiration_days"].(int))}
		noncurrentVersionTransitionDays := lifecycle.NoncurrentVersionTransition{NoncurrentDays: lifecycle.ExpirationDays(rule["noncurrent_version_transition_days"].(int))}
		tags := map[string]string{}
		for k, v := range rule["tags"].(map[string]interface{}) {
			tags[k] = v.(string)
		}

		if len(tags) > 0 {
			filter.And.Prefix = rule["filter"].(string)
			for k, v := range tags {
				filter.And.Tags = append(filter.And.Tags, lifecycle.Tag{Key: k, Value: v})
			}
		} else {
			filter.Prefix = rule["filter"].(string)
		}

		r := lifecycle.Rule{
			ID:                          rule["id"].(string),
			Expiration:                  parseILMExpiration(rule["expiration"].(string)),
			Transition:                  parseILMTransition(rule["transition"].([]interface{})),
			NoncurrentVersionExpiration: noncurrentVersionExpirationDays,
			NoncurrentVersionTransition: noncurrentVersionTransitionDays,
			Status:                      "Enabled",
			RuleFilter:                  filter,
		}

		config.Rules = append(config.Rules, r)
	}

	if err := c.SetBucketLifecycle(ctx, bucket, config); err != nil {
		return NewResourceError("creating bucket lifecycle failed", bucket, err)
	}

	d.SetId(bucket)

	return minioReadILMPolicy(ctx, d, meta)
}

func minioReadILMPolicy(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c := meta.(*S3MinioClient).S3Client

	rules := make([]map[string]interface{}, 0)
	config, err := c.GetBucketLifecycle(ctx, d.Id())
	if err != nil {
		// TODO: distinguish between error and 404 not found
		log.Println(NewResourceErrorStr("reading lifecycle configuration failed", d.Id(), err))
		d.SetId("")
		return nil
	}

	if err = d.Set("bucket", d.Id()); err != nil {
		return NewResourceError("setting bucket failed", d.Id(), err)
	}

	for _, r := range config.Rules {
		var expiration string

		if r.Expiration.DeleteMarker {
			expiration = "DeleteMarker"
		} else if r.Expiration.Days != 0 {
			expiration = fmt.Sprintf("%dd", r.Expiration.Days)
		} else if !r.Expiration.IsNull() {
			expiration = r.Expiration.Date.Format("2006-01-02")
		}

		transitions := make([]map[string]string, 0)

		if !r.Transition.IsNull() {
			transition := map[string]string{}
			if !r.Transition.IsDaysNull() {
				transition["days"] = fmt.Sprintf("%dd", r.Transition.Days)
			} else if !r.Transition.IsDateNull() {
				transition["date"] = r.Transition.Date.Format("2006-01-02")
			}
			transition["storage_class"] = r.Transition.StorageClass
			transitions = append(transitions, transition)

		}

		var noncurrentVersionExpirationDays int
		if r.NoncurrentVersionExpiration.NoncurrentDays != 0 {
			noncurrentVersionExpirationDays = int(r.NoncurrentVersionExpiration.NoncurrentDays)
		}

		var noncurrentVersionTransitionDays int
		if r.NoncurrentVersionTransition.NoncurrentDays != 0 {
			noncurrentVersionTransitionDays = int(r.NoncurrentVersionTransition.NoncurrentDays)
		}

		var prefix string
		tags := map[string]string{}
		if len(r.RuleFilter.And.Tags) > 0 {
			prefix = r.RuleFilter.And.Prefix
			for _, tag := range r.RuleFilter.And.Tags {
				tags[tag.Key] = tag.Value
			}
		} else {
			prefix = r.RuleFilter.Prefix
		}

		rule := map[string]interface{}{
			"id":                                 r.ID,
			"expiration":                         expiration,
			"transition":                         transitions,
			"noncurrent_version_expiration_days": noncurrentVersionExpirationDays,
			"noncurrent_version_transition_days": noncurrentVersionTransitionDays,
			"status":                             r.Status,
			"filter":                             prefix,
			"tags":                               tags,
		}

		rules = append(rules, rule)
	}

	if err := d.Set("rule", rules); err != nil {
		return NewResourceError("reading lifecycle configuration failed", d.Id(), err)
	}

	return nil
}

func minioUpdateILMPolicy(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	if d.HasChange("rule") {
		minioCreateILMPolicy(ctx, d, meta)
	}

	return minioReadILMPolicy(ctx, d, meta)
}

func minioDeleteILMPolicy(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	c := meta.(*S3MinioClient).S3Client

	config := lifecycle.NewConfiguration()

	if err := c.SetBucketLifecycle(ctx, d.Id(), config); err != nil {
		return NewResourceError("deleting lifecycle configuration failed", d.Id(), err)
	}

	d.SetId("")

	return nil
}

func parseILMExpiration(s string) lifecycle.Expiration {
	var days int
	if s == "DeleteMarker" {
		return lifecycle.Expiration{DeleteMarker: true}
	}
	if _, err := fmt.Sscanf(s, "%dd", &days); err == nil {
		return lifecycle.Expiration{Days: lifecycle.ExpirationDays(days)}
	}
	if date, err := time.Parse("2006-01-02", s); err == nil {
		return lifecycle.Expiration{Date: lifecycle.ExpirationDate{Time: date}}
	}

	return lifecycle.Expiration{}
}

func parseILMTransition(transition interface{}) lifecycle.Transition {
	transitions := transition.([]interface{})
	if len(transitions) == 0 {
		return lifecycle.Transition{}
	}
	t := transitions[0].(map[string]interface{})
	var days int
	if _, err := fmt.Sscanf(t["days"].(string), "%dd", &days); err == nil {
		return lifecycle.Transition{Days: lifecycle.ExpirationDays(days), StorageClass: t["storage_class"].(string)}
	}
	if date, err := time.Parse("2006-01-02", t["date"].(string)); err == nil {
		return lifecycle.Transition{Date: lifecycle.ExpirationDate{Time: date}, StorageClass: t["storage_class"].(string)}
	}

	return lifecycle.Transition{}
}
