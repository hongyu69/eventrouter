// Sample UDA which sums incoming values.
function main() {
    this.init = function () {
        this.clusters = [];
    }

    this.accumulate = function (value, timestamp) {
        if (value && this.clusters.indexOf(value) === -1) {
            this.clusters.push(value);
        }
    }

    /*this.deaccumulate = function (value, timestamp) {
        this.state -= value;
    }

    this.deaccumulateState = function (otherState) {
            this.state -= otherState.state;
    }*/

    this.computeResult = function () {
        return this.clusters.join(", ");
    }
}