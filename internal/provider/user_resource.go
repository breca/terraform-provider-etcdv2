package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"go.etcd.io/etcd/client/v2"
)

var (
	_ resource.Resource                = &userResource{}
	_ resource.ResourceWithImportState = &userResource{}
)

func NewUserResource() resource.Resource {
	return &userResource{}
}

type userResource struct {
	client *client.Client
}

type userResourceModel struct {
	Username types.String   `tfsdk:"username"`
	Password types.String   `tfsdk:"password"`
	Roles    []types.String `tfsdk:"roles"`
}

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages an etcdv2 user.",
		Attributes: map[string]schema.Attribute{
			"username": schema.StringAttribute{
				Description: "The username for the etcd user.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"password": schema.StringAttribute{
				Description: "The password for the etcd user.",
				Required:    true,
				Sensitive:   true,
			},
			"roles": schema.ListAttribute{
				Description: "List of roles assigned to the user.",
				Optional:    true,
				ElementType: types.StringType,
			},
		},
	}
}

func (r *userResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected client.Client, got: %T", req.ProviderData),
		)
		return
	}

	r.client = client
}

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	authUserAPI := client.NewAuthUserAPI(*r.client)

	// Create user
	err := authUserAPI.AddUser(ctx, plan.Username.ValueString(), plan.Password.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(
			"Error creating user",
			fmt.Sprintf("Could not create user %s: %s", plan.Username.ValueString(), err),
		)
		return
	}

	// Grant roles if specified
	if len(plan.Roles) > 0 {
		roles := make([]string, 0, len(plan.Roles))
		for _, role := range plan.Roles {
			if role.ValueString() != "" {
				roles = append(roles, role.ValueString())
			}
		}
		if len(roles) > 0 {
			_, err := authUserAPI.GrantUser(ctx, plan.Username.ValueString(), roles)
			if err != nil {
				resp.Diagnostics.AddError(
					"Error granting roles to user",
					fmt.Sprintf("Could not grant roles to user %s: %s", plan.Username.ValueString(), err),
				)
				return
			}
		}
	}

	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	authUserAPI := client.NewAuthUserAPI(*r.client)

	// Get user details
	user, err := authUserAPI.GetUser(ctx, state.Username.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(
			"Error reading user",
			fmt.Sprintf("Could not read user %s: %s", state.Username.ValueString(), err),
		)
		return
	}

	filtered := filterEmptyRoles(user.Roles)

	// Update roles
	state.Roles = make([]types.String, len(filtered))
	for i, role := range user.Roles {
		state.Roles[i] = types.StringValue(role)
	}

	// Password is write-only, so we keep the existing value from state
	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan userResourceModel
	var state userResourceModel

	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	diags = req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	authUserAPI := client.NewAuthUserAPI(*r.client)

	// Update password if changed
	if !plan.Password.Equal(state.Password) {
		_, err := authUserAPI.ChangePassword(ctx, plan.Username.ValueString(), plan.Password.ValueString())
		if err != nil {
			resp.Diagnostics.AddError(
				"Error updating user password",
				fmt.Sprintf("Could not update password for user %s: %s", plan.Username.ValueString(), err),
			)
			return
		}
	}

	// Update roles
	// First, revoke all existing roles (only non-empty roles)
	if len(state.Roles) > 0 {
		rawRoles := make([]string, 0, len(state.Roles))
		for _, role := range state.Roles {
			rawRoles = append(rawRoles, role.ValueString())
		}
		roles := filterEmptyRoles(rawRoles)
		if len(roles) > 0 {
			_, err := authUserAPI.RevokeUser(ctx, plan.Username.ValueString(), roles)
			if err != nil {
				resp.Diagnostics.AddError(
					"Error revoking user roles",
					fmt.Sprintf("Could not revoke roles from user %s: %s", plan.Username.ValueString(), err),
				)
				return
			}
		}
	}

	// Then grant new roles (only non-empty roles)
	if len(plan.Roles) > 0 {
		roles := make([]string, 0, len(plan.Roles))
		for _, role := range plan.Roles {
			if role.ValueString() != "" {
				roles = append(roles, role.ValueString())
			}
		}
		if len(roles) > 0 {
			_, err := authUserAPI.GrantUser(ctx, plan.Username.ValueString(), roles)
			if err != nil {
				resp.Diagnostics.AddError(
					"Error granting user roles",
					fmt.Sprintf("Could not grant roles to user %s: %s", plan.Username.ValueString(), err),
				)
				return
			}
		}
	}

	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	authUserAPI := client.NewAuthUserAPI(*r.client)

	err := authUserAPI.RemoveUser(ctx, state.Username.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(
			"Error deleting user",
			fmt.Sprintf("Could not delete user %s: %s", state.Username.ValueString(), err),
		)
		return
	}
}

func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by username
	resource.ImportStatePassthroughID(ctx, path.Root("username"), req, resp)

	// Note: Password cannot be imported as it's not retrievable from etcd
	// Users will need to set the password in their configuration after import
	resp.Diagnostics.AddWarning(
		"Password Required After Import",
		"The password attribute cannot be imported from etcd. Please set the password in your configuration.",
	)
}

func filterEmptyRoles(roles []string) []string {
	result := make([]string, 0, len(roles))
	for _, r := range roles {
		if r != "" {
			result = append(result, r)
		}
	}
	return result
}
