#!/usr/bin/env python3
# Tiny MNIST training demo for the Nimbus GPU plane.
#
# Drop-in: the gx10 worker runs this inside a pytorch/pytorch container with
# `--gpus all`, so torch.cuda.is_available() returns True on the GX10.
# Trains a 2-layer MLP for one epoch (~30s on a Blackwell, longer the first
# time because torchvision downloads MNIST). Prints loss per 100 steps and a
# final accuracy on the test set.
#
# Run via:
#   gx10 submit pytorch/pytorch:2.4.0-cuda12.1-cudnn9-runtime -- \
#     bash -c "pip install -q torchvision && curl -fsSL ${NIMBUS_GPU_API%/api/gpu}/api/gpu/scripts/demo-mnist.py | python -"
#
# (The worker passes ${NIMBUS_GPU_API} into the container env automatically.)

import torch
import torch.nn as nn
import torch.optim as optim
from torchvision import datasets, transforms
from torch.utils.data import DataLoader

device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
print(f"device: {device}")
if device.type == "cuda":
    print(f"  name: {torch.cuda.get_device_name(0)}")
    print(f"  arch: {torch.cuda.get_device_capability(0)}")

# Dataset — torchvision auto-downloads ~10 MB the first time.
tfm = transforms.Compose([transforms.ToTensor(), transforms.Normalize((0.1307,), (0.3081,))])
train_ds = datasets.MNIST("/tmp/mnist", train=True, download=True, transform=tfm)
test_ds = datasets.MNIST("/tmp/mnist", train=False, download=True, transform=tfm)
train_loader = DataLoader(train_ds, batch_size=128, shuffle=True, num_workers=2)
test_loader = DataLoader(test_ds, batch_size=512, num_workers=2)

# Tiny MLP. ~100k params; finishes one epoch in seconds on any GPU and is
# enough to demonstrate training-loop hygiene without dragging in a CNN.
model = nn.Sequential(
    nn.Flatten(),
    nn.Linear(28 * 28, 128),
    nn.ReLU(),
    nn.Linear(128, 64),
    nn.ReLU(),
    nn.Linear(64, 10),
).to(device)

opt = optim.Adam(model.parameters(), lr=1e-3)
loss_fn = nn.CrossEntropyLoss()

print("training:")
model.train()
for step, (x, y) in enumerate(train_loader):
    x, y = x.to(device), y.to(device)
    opt.zero_grad()
    loss = loss_fn(model(x), y)
    loss.backward()
    opt.step()
    if step % 100 == 0:
        print(f"  step {step:4d}  loss {loss.item():.4f}")

print("evaluating:")
model.eval()
correct = total = 0
with torch.no_grad():
    for x, y in test_loader:
        x, y = x.to(device), y.to(device)
        pred = model(x).argmax(dim=1)
        correct += (pred == y).sum().item()
        total += y.size(0)
print(f"test accuracy: {correct / total:.4f}  ({correct}/{total})")
