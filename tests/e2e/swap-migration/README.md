# swap-migration

This test case checks that we're able to update VMs from the old swap format to the new one.

This will _eventually_ come in two phases:

1. Switch from `.Swap: *resource.Quantity` to `.SwapV2: *SwapInfo`
2. Switch from `.SwapV2: *SwapInfo` to `.Swap: *resource.Quantity`

Once all VirtualMachine objects have been switched to `SwapV2`, we'll be free to change the type of
`Swap` and migrate back.
