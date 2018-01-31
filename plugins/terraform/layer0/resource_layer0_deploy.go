package layer0

import (
	"log"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/quintilesims/layer0/client"
	"github.com/quintilesims/layer0/common/errors"
	"github.com/quintilesims/layer0/common/models"
)

func resourceLayer0Deploy() *schema.Resource {
	return &schema.Resource{
		Create: resourceLayer0DeployCreate,
		Read:   resourceLayer0DeployRead,
		Delete: resourceLayer0DeployDelete,
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},
		Schema: map[string]*schema.Schema{
			"name": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"content": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"version": {
				Type:     schema.TypeString,
				Computed: true,
			},
		},
	}
}

func resourceLayer0DeployCreate(d *schema.ResourceData, meta interface{}) error {
	apiClient := meta.(client.Client)

	req := models.CreateDeployRequest{
		DeployName: d.Get("name").(string),
		DeployFile: []byte(d.Get("content").(string)),
	}

	deployID, err := apiClient.CreateDeploy(req)
	if err != nil {
		return err
	}

	d.SetId(deployID)

	return resourceLayer0DeployRead(d, meta)
}

func resourceLayer0DeployRead(d *schema.ResourceData, meta interface{}) error {
	apiClient := meta.(client.Client)
	deployID := d.Id()

	deploy, err := apiClient.ReadDeploy(deployID)
	if err != nil {
		if err, ok := err.(*errors.ServerError); ok && err.Code == errors.DeployDoesNotExist {
			d.SetId("")
			log.Printf("[WARN] Error Reading Deploy (%s), deploy does not exist", deployID)
			return nil
		}

		return err
	}

	// do not set content as it fails to properly diff
	d.Set("name", deploy.DeployName)
	d.Set("version", deploy.Version)

	return nil
}

func resourceLayer0DeployDelete(d *schema.ResourceData, meta interface{}) error {
	apiClient := meta.(client.Client)
	deployID := d.Id()

	if err := apiClient.DeleteDeploy(deployID); err != nil {
		if err, ok := err.(*errors.ServerError); ok && err.Code == errors.DeployDoesNotExist {
			return nil
		}

		return err
	}

	return nil
}
