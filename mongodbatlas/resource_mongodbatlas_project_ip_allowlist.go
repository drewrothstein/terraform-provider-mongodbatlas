package mongodbatlas

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/helper/validation"
	matlas "go.mongodb.org/atlas/mongodbatlas"
)

const (
	errorAllowlistCreate = "error creating Project IP Allowlist information: %s"
	errorAllowlistRead   = "error getting Project IP Allowlist information: %s"
	// errorAllowlistUpdate  = "error updating Project IP Allowlist information: %s"
	errorAllowlistDelete  = "error deleting Project IP Allowlist information: %s"
	errorAllowlistSetting = "error setting `%s` for Project IP Allowlist (%s): %s"
)

func resourceMongoDBAtlasProjectIPAllowlist() *schema.Resource {
	return &schema.Resource{
		Create: resourceMongoDBAtlasProjectIPAllowlistCreate,
		Read:   resourceMongoDBAtlasProjectIPAllowlistRead,
		Delete: resourceMongoDBAtlasProjectIPAllowlistDelete,
		Importer: &schema.ResourceImporter{
			State: resourceMongoDBAtlasIPAllowlistImportState,
		},
		Schema: map[string]*schema.Schema{
			"project_id": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"cidr_block": {
				Type:          schema.TypeString,
				Optional:      true,
				Computed:      true,
				ForceNew:      true,
				ConflictsWith: []string{"aws_security_group", "ip_address"},
				ValidateFunc: func(i interface{}, k string) (s []string, es []error) {
					v, ok := i.(string)
					if !ok {
						es = append(es, fmt.Errorf("expected type of %s to be string", k))
						return
					}

					_, ipnet, err := net.ParseCIDR(v)
					if err != nil {
						es = append(es, fmt.Errorf("expected %s to contain a valid CIDR, got: %s with err: %s", k, v, err))
						return
					}

					if ipnet == nil || v != ipnet.String() {
						es = append(es, fmt.Errorf("expected %s to contain a valid network CIDR, expected %s, got %s", k, ipnet, v))
						return
					}
					return
				},
			},
			"ip_address": {
				Type:          schema.TypeString,
				Optional:      true,
				Computed:      true,
				ForceNew:      true,
				ConflictsWith: []string{"aws_security_group", "cidr_block"},
				ValidateFunc:  validation.IsIPAddress,
			},
			// You must configure VPC peering for your project before you can allowlist an AWS security group.
			"aws_security_group": {
				Type:          schema.TypeString,
				Optional:      true,
				Computed:      true,
				ForceNew:      true,
				ConflictsWith: []string{"ip_address", "cidr_block"},
			},
			"comment": {
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ForceNew:     true,
				ValidateFunc: validation.NoZeroValues,
			},
		},
		Timeouts: &schema.ResourceTimeout{
			Read:   schema.DefaultTimeout(45 * time.Minute),
			Delete: schema.DefaultTimeout(45 * time.Minute),
		},
	}
}

func resourceMongoDBAtlasProjectIPAllowlistCreate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*matlas.Client)
	projectID := d.Get("project_id").(string)
	cirdBlock := d.Get("cidr_block").(string)
	ipAddress := d.Get("ip_address").(string)
	awsSecurityGroup := d.Get("aws_security_group").(string)

	if cirdBlock == "" && ipAddress == "" && awsSecurityGroup == "" {
		return errors.New("cidr_block, ip_address or aws_security_group needs to contain a value")
	}

	stateConf := &resource.StateChangeConf{
		Pending: []string{"pending"},
		Target:  []string{"created", "failed"},
		Refresh: func() (interface{}, string, error) {
			allowlist, _, err := conn.ProjectIPWhitelist.Create(context.Background(), projectID, []*matlas.ProjectIPWhitelist{ // TODO: Language Inclusivity
				{
					AwsSecurityGroup: awsSecurityGroup,
					CIDRBlock:        cirdBlock,
					IPAddress:        ipAddress,
					Comment:          d.Get("comment").(string),
				},
			})
			if err != nil {
				if strings.Contains(fmt.Sprint(err), "Unexpected error") ||
					strings.Contains(fmt.Sprint(err), "UNEXPECTED_ERROR") ||
					strings.Contains(fmt.Sprint(err), "500") {
					return nil, "pending", nil
				}
				return nil, "failed", fmt.Errorf(errorAllowlistCreate, err)
			}

			if len(allowlist) > 0 {
				for _, entry := range allowlist {
					if entry.IPAddress == ipAddress || entry.CIDRBlock == cirdBlock {
						return allowlist, "created", nil
					}
				}
				return nil, "pending", nil
			}

			return allowlist, "created", nil
		},
		Timeout:    45 * time.Minute,
		Delay:      30 * time.Second,
		MinTimeout: 10 * time.Second,
	}

	// Wait, catching any errors
	_, err := stateConf.WaitForState()
	if err != nil {
		return fmt.Errorf(errorPeersCreate, err)
	}

	var entry string
	switch {
	case cirdBlock != "":
		entry = cirdBlock
	case ipAddress != "":
		entry = ipAddress
	default:
		entry = awsSecurityGroup
	}

	d.SetId(encodeStateID(map[string]string{
		"project_id": projectID,
		"entry":      entry,
	}))

	return resourceMongoDBAtlasProjectIPAllowlistRead(d, meta)
}

func resourceMongoDBAtlasProjectIPAllowlistRead(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*matlas.Client)
	ids := decodeStateID(d.Id())

	return resource.Retry(2*time.Minute, func() *resource.RetryError {

		allowlist, _, err := conn.ProjectIPWhitelist.Get(context.Background(), ids["project_id"], ids["entry"]) // TODO: Language Inclusivity
		if err != nil {
			switch {
			case strings.Contains(fmt.Sprint(err), "500"):
				return resource.RetryableError(err)
			case strings.Contains(fmt.Sprint(err), "404"):
				d.SetId("")
				return nil
			default:
				return resource.NonRetryableError(fmt.Errorf(errorAllowlistRead, err))
			}
		}

		if allowlist != nil {
			if err := d.Set("aws_security_group", allowlist.AwsSecurityGroup); err != nil {
				return resource.NonRetryableError(fmt.Errorf(errorAllowlistSetting, "aws_security_group", ids["project_id"], err))
			}
			if err := d.Set("cidr_block", allowlist.CIDRBlock); err != nil {
				return resource.NonRetryableError(fmt.Errorf(errorAllowlistSetting, "cidr_block", ids["project_id"], err))
			}
			if err := d.Set("ip_address", allowlist.IPAddress); err != nil {
				return resource.NonRetryableError(fmt.Errorf(errorAllowlistSetting, "ip_address", ids["project_id"], err))
			}
			if err := d.Set("comment", allowlist.Comment); err != nil {
				return resource.NonRetryableError(fmt.Errorf(errorAllowlistSetting, "comment", ids["project_id"], err))
			}
		}
		return nil
	})
}

func resourceMongoDBAtlasProjectIPAllowlistDelete(d *schema.ResourceData, meta interface{}) error {
	//Get the client connection.
	conn := meta.(*matlas.Client)
	ids := decodeStateID(d.Id())

	return resource.Retry(5*time.Minute, func() *resource.RetryError {
		_, err := conn.ProjectIPWhitelist.Delete(context.Background(), ids["project_id"], ids["entry"]) // TODO: Language Inclusivity
		if err != nil {
			if strings.Contains(fmt.Sprint(err), "500") ||
				strings.Contains(fmt.Sprint(err), "Unexpected error") ||
				strings.Contains(fmt.Sprint(err), "UNEXPECTED_ERROR") {
				return resource.RetryableError(err)
			}
			return resource.NonRetryableError(fmt.Errorf(errorAllowlistDelete, err))
		}

		entry, _, err := conn.ProjectIPWhitelist.Get(context.Background(), ids["project_id"], ids["entry"]) // TODO: Language Inclusivity
		if err != nil {
			if strings.Contains(fmt.Sprint(err), "404") ||
				strings.Contains(fmt.Sprint(err), "ATLAS_ALLOWLIST_NOT_FOUND") {
				return nil
			}
			return resource.RetryableError(err)
		}
		if entry != nil {
			_, err := conn.ProjectIPWhitelist.Delete(context.Background(), ids["project_id"], ids["entry"]) // TODO: Language Inclusivity
			if err != nil {
				if strings.Contains(fmt.Sprint(err), "500") ||
					strings.Contains(fmt.Sprint(err), "Unexpected error") ||
					strings.Contains(fmt.Sprint(err), "UNEXPECTED_ERROR") {
					return resource.RetryableError(err)
				}
				return resource.NonRetryableError(fmt.Errorf(errorAllowlistDelete, err))
			}
		}
		return nil
	})
}

func resourceMongoDBAtlasIPAllowlistImportState(d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	conn := meta.(*matlas.Client)

	parts := strings.SplitN(d.Id(), "-", 2)
	if len(parts) != 2 {
		return nil, errors.New("import format error: to import a peer, use the format {project_id}-{allowlist_entry}")
	}

	projectID := parts[0]
	entry := parts[1]

	_, _, err := conn.ProjectIPWhitelist.Get(context.Background(), projectID, entry) // TODO: Language Inclusivity
	if err != nil {
		return nil, fmt.Errorf("couldn't import entry allowlist %s in project %s, error: %s", entry, projectID, err)
	}

	if err := d.Set("project_id", projectID); err != nil {
		log.Printf("[WARN] Error setting project_id for (%s): %s", projectID, err)
	}

	d.SetId(encodeStateID(map[string]string{
		"project_id": projectID,
		"entry":      entry,
	}))

	return []*schema.ResourceData{d}, nil
}