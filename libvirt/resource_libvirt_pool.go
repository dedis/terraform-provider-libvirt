package libvirt

import (
	"encoding/xml"
	"fmt"
	"log"

	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	libvirt "github.com/libvirt/libvirt-go"
	libvirtxml "github.com/libvirt/libvirt-go-xml"
)

func resourceLibvirtPool() *schema.Resource {
	return &schema.Resource{
		Create: resourceLibvirtPoolCreate,
		Read:   resourceLibvirtPoolRead,
		Delete: resourceLibvirtPoolDelete,
		Exists: resourceLibvirtPoolExists,
		Schema: map[string]*schema.Schema{
			"name": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"type": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"capacity": {
				Type:     schema.TypeInt,
				Optional: true,
				Computed: true,
				ForceNew: true,
			},
			"allocation": {
				Type:     schema.TypeInt,
				Optional: true,
				Computed: true,
				ForceNew: true,
			},
			"available": {
				Type:     schema.TypeString,
				Computed: true,
				Optional: true,
				ForceNew: true,
			},
			"xml": {
				Type:     schema.TypeList,
				Optional: true,
				MaxItems: 1,
				ForceNew: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"xslt": {
							Type:     schema.TypeString,
							Optional: true,
							ForceNew: true,
						},
					},
				},
			},

			// Dir-specific attributes
			"path": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
			},

			// logical-specific attributes
			"source_devices": {
				Type:     schema.TypeList,
				Optional: true,
				ForceNew: true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},
		},
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},
	}
}

func resourceLibvirtPoolCreate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Client)
	if client.libvirt == nil {
		return fmt.Errorf(LibVirtConIsNil)
	}

	poolType := d.Get("type").(string)
	if poolType != "dir" && poolType != "logical" {
		return fmt.Errorf("Only storage pools of type \"dir\" and \"logical\" are supported")
	}

	poolName := d.Get("name").(string)

	client.poolMutexKV.Lock(poolName)
	defer client.poolMutexKV.Unlock(poolName)

	// Check whether the storage pool already exists. Its name needs to be
	// unique.
	if _, err := client.libvirt.LookupStoragePoolByName(poolName); err == nil {
		return fmt.Errorf("storage pool '%s' already exists", poolName)
	}
	log.Printf("[DEBUG] Pool with name '%s' does not exist yet", poolName)

	poolPath := d.Get("path").(string)

	var poolDef *libvirtxml.StoragePool
	// In some cases we don't need to build the pool
	needToBuild := true

	if poolType == "dir" {
		if poolPath == "" {
			return fmt.Errorf("\"path\" attribute is requires for storage pools of type \"dir\"")
		}

		sourceDevices := d.Get("source_devices.#").(int)
		if sourceDevices != 0 {
			return fmt.Errorf("\"source_devices\" attribute cannot be used for storage pool of type \"dir\"")
		}

		poolDef = &libvirtxml.StoragePool{
			Type: "dir",
			Name: poolName,
			Target: &libvirtxml.StoragePoolTarget{
				Path: poolPath,
			},
		}
	} else if poolType == "logical" {
		// path is auto-generated for lvm pools, so we don't set/read it
		if poolPath != "" {
			return fmt.Errorf("\"path\" attribute cannot be used for storage pool of type \"logical\"")
		}

		poolDef = &libvirtxml.StoragePool{
			Type: "logical",
			Name: poolName,
		}

		var devices []libvirtxml.StoragePoolSourceDevice

		for i := 0; i < d.Get("source_devices.#").(int); i++ {
			device := d.Get(fmt.Sprintf("source_devices.%d", i)).(string)
			devices = append(devices, libvirtxml.StoragePoolSourceDevice{Path: device})
		}

		if devices != nil {
			poolDef.Source = &libvirtxml.StoragePoolSource{
				Device: devices,
			}
		} else {
			// if no source device given for logical pool, we don't need to build, just use the existing vg
			needToBuild = false
		}
	}

	data, err := xmlMarshallIndented(poolDef)
	if err != nil {
		return fmt.Errorf("Error serializing libvirt storage pool: %s", err)
	}
	log.Printf("[DEBUG] Generated XML for libvirt storage pool:\n%s", data)

	data, err = transformResourceXML(data, d)
	if err != nil {
		return fmt.Errorf("Error applying XSLT stylesheet: %s", err)
	}

	// create the pool
	pool, err := client.libvirt.StoragePoolDefineXML(data, 0)
	if err != nil {
		return fmt.Errorf("Error creating libvirt storage pool: %s", err)
	}
	defer pool.Free()

	if needToBuild {
		err = pool.Build(0)
		if err != nil {
			return fmt.Errorf("Error building libvirt storage pool: %s", err)
		}
	}

	err = pool.SetAutostart(true)
	if err != nil {
		return fmt.Errorf("Error setting up libvirt storage pool: %s", err)
	}

	err = pool.Create(0)
	if err != nil {
		return fmt.Errorf("Error starting libvirt storage pool: %s", err)
	}

	err = pool.Refresh(0)
	if err != nil {
		return fmt.Errorf("Error refreshing libvirt storage pool: %s", err)
	}

	id, err := pool.GetUUIDString()
	if err != nil {
		return fmt.Errorf("Error retrieving libvirt pool id: %s", err)
	}
	d.SetId(id)

	// make sure we record the id even if the rest of this gets interrupted
	d.Partial(true)
	d.Set("id", id)
	d.SetPartial("id")
	d.Partial(false)

	log.Printf("[INFO] Pool ID: %s", d.Id())

	if err := poolWaitForExists(client.libvirt, id); err != nil {
		return err
	}

	return resourceLibvirtPoolRead(d, meta)
}

func resourceLibvirtPoolRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Client)
	virConn := client.libvirt
	if virConn == nil {
		return fmt.Errorf(LibVirtConIsNil)
	}

	pool, err := virConn.LookupStoragePoolByUUIDString(d.Id())
	if pool == nil {
		log.Printf("storage pool '%s' may have been deleted outside Terraform", d.Id())
		d.SetId("")
		return nil
	}
	defer pool.Free()

	poolName, err := pool.GetName()
	if err != nil {
		return fmt.Errorf("error retrieving pool name: %s", err)
	}
	d.Set("name", poolName)

	info, err := pool.GetInfo()
	if err != nil {
		return fmt.Errorf("error retrieving pool info: %s", err)
	}
	d.Set("capacity", info.Capacity)
	d.Set("allocation", info.Allocation)
	d.Set("available", info.Available)

	poolDefXML, err := pool.GetXMLDesc(0)
	if err != nil {
		return fmt.Errorf("could not get XML description for pool %s: %s", poolName, err)
	}

	var poolDef libvirtxml.StoragePool
	err = xml.Unmarshal([]byte(poolDefXML), &poolDef)
	if err != nil {
		return fmt.Errorf("could not get a pool definition from XML for %s: %s", poolDef.Name, err)
	}

	var poolPath string
	if poolDef.Target != nil && poolDef.Target.Path != "" {
		poolPath = poolDef.Target.Path
	}

	// for logical pool the path auto-generated, so we don't set/read it
	if poolDef.Type != "logical" {
		if poolPath == "" {
			log.Printf("Pool %s has no path specified", poolName)
		} else {
			log.Printf("[DEBUG] Pool %s path: %s", poolName, poolPath)
			d.Set("path", poolPath)
		}
	}

	poolType := poolDef.Type
	if poolType == "" {
		log.Printf("Pool %s has no type specified", poolName)
	} else {
		log.Printf("[DEBUG] Pool %s type: %s", poolName, poolType)
		d.Set("type", poolType)
	}

	return nil
}

func resourceLibvirtPoolDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Client)
	if client.libvirt == nil {
		return fmt.Errorf(LibVirtConIsNil)
	}

	return deletePool(client, d.Id())
}

func resourceLibvirtPoolExists(d *schema.ResourceData, meta interface{}) (bool, error) {
	log.Printf("[DEBUG] Check if resource (id : %s) libvirt_pool exists", d.Id())
	client := meta.(*Client)
	virConn := client.libvirt
	if virConn == nil {
		return false, fmt.Errorf(LibVirtConIsNil)
	}

	pool, err := virConn.LookupStoragePoolByUUIDString(d.Id())
	if err != nil {
		virErr := err.(libvirt.Error)
		if virErr.Code != libvirt.ERR_NO_STORAGE_POOL {
			return false, fmt.Errorf("Can't retrieve pool %s", d.Id())
		}
		// does not exist, but no error
		return false, nil
	}
	defer pool.Free()

	return true, nil
}
