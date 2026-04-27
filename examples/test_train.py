import argparse

parser = argparse.ArgumentParser()
parser.add_argument("--lr", type=float, default=0.001,
                    help="learning rate")
parser.add_argument("--batch-size", type=int, default=32)
parser.add_argument("--optimizer", default="adam")
parser.add_argument("--resume", action="store_true")
parser.add_argument("epochs", type=int)  # positional → skip